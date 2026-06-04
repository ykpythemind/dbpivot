package proxy

import (
	"fmt"
	"net"
	"runtime"

	"github.com/xdg-go/scram"
)

// MongoDB upstream authentication (proxy→upstream leg).
//
// This is the MongoDB analog of auth.go's AuthenticateUpstream (PG) and
// mysqlauth.go's AuthenticateUpstreamMySQL: the proxy logs in to the real
// mongod with the configured credentials. As noted in the handshake design,
// the proxy advertises an auth-disabled standalone to its own clients (SCRAM
// "trust" auth is impossible client-side), so all authentication happens here,
// on the upstream leg.
//
// MongoDB does not have a dedicated auth phase in the wire envelope the way PG
// (Authentication messages) and MySQL (auth-switch packets) do. Instead SCRAM
// runs as ordinary commands: the SCRAM client-first / client-final messages are
// carried as the `payload` of `saslStart` / `saslContinue` commands sent over
// OP_MSG, and the server's SCRAM messages come back as the `payload` of the
// command replies. The actual SCRAM-SHA-256 cryptography is delegated to the
// xdg-go/scram client already used by the PostgreSQL path.
//
// Like the PG/MySQL upstream-auth helpers this is a pure function over a
// net.Conn: it owns no server state and reads/writes the conn directly, so it
// can be unit-tested against a fake mongod and composed into the dispatcher.

// mongoScramMechanism is the only SASL mechanism dbpivot drives upstream. It
// matches the PostgreSQL path's SCRAM-SHA-256-only support; SCRAM-SHA-1 and
// x.509 are out of scope for now.
const mongoScramMechanism = "SCRAM-SHA-256"

// AuthenticateUpstreamMongo drives a SCRAM-SHA-256 login against an upstream
// mongod using the saslStart/saslContinue command exchange over OP_MSG. authDB
// is the authentication database each sasl command runs against (the $db field;
// defaults to "admin", which is where MongoDB user credentials normally live).
//
// It returns nil once the upstream reports the conversation done with ok:1, or
// an error on any wire failure, command error (ok:0 — e.g. authentication
// failed), or SCRAM verification failure. The connection is left positioned
// right after the final reply, ready for the raw command-phase pipe.
func AuthenticateUpstreamMongo(conn net.Conn, user, password, authDB string) error {
	if authDB == "" {
		authDB = "admin"
	}

	// MongoDB requires the connection handshake — a `hello` carrying client
	// metadata — as the very first command on a connection: a real mongod closes
	// the connection outright if the first command is anything else (e.g.
	// saslStart), surfacing here as an EOF reading the saslStart reply. So
	// perform the handshake before driving SCRAM. Only ok:1 matters; the proxy
	// already committed to a wire version in its client-facing hello, so the
	// upstream's advertised limits are intentionally ignored.
	if err := mongoUpstreamHandshake(conn, authDB); err != nil {
		return fmt.Errorf("mongo upstream handshake: %w", err)
	}

	client, err := scram.SHA256.NewClient(user, password, "")
	if err != nil {
		return fmt.Errorf("mongo scram client: %w", err)
	}
	conv := client.NewConversation()

	clientFirst, err := conv.Step("")
	if err != nil {
		return fmt.Errorf("mongo scram client-first: %w", err)
	}

	// saslStart carries the SCRAM client-first message. options.skipEmptyExchange
	// asks modern servers (4.4+) to fold the trailing empty round trip into the
	// server-final reply; the loop below still handles servers that don't honor it.
	reply, err := mongoCommandRoundTrip(conn, BSON{
		{Key: "saslStart", Value: int32(1)},
		{Key: "mechanism", Value: mongoScramMechanism},
		{Key: "payload", Value: BSONBinary{Subtype: BinarySubtypeGeneric, Data: []byte(clientFirst)}},
		{Key: "options", Value: BSON{{Key: "skipEmptyExchange", Value: true}}},
		{Key: "$db", Value: authDB},
	})
	if err != nil {
		return fmt.Errorf("mongo saslStart: %w", err)
	}
	convID, serverFirst, _, err := parseMongoSaslReply(reply)
	if err != nil {
		return fmt.Errorf("mongo saslStart: %w", err)
	}

	clientFinal, err := conv.Step(string(serverFirst))
	if err != nil {
		return fmt.Errorf("mongo scram client-final: %w", err)
	}

	// saslContinue carries the SCRAM client-final message (with the proof). The
	// reply's payload is the server-final message (with the ServerSignature).
	reply, err = mongoCommandRoundTrip(conn, BSON{
		{Key: "saslContinue", Value: int32(1)},
		{Key: "conversationId", Value: convID},
		{Key: "payload", Value: BSONBinary{Subtype: BinarySubtypeGeneric, Data: []byte(clientFinal)}},
		{Key: "$db", Value: authDB},
	})
	if err != nil {
		return fmt.Errorf("mongo saslContinue: %w", err)
	}
	_, serverFinal, done, err := parseMongoSaslReply(reply)
	if err != nil {
		return fmt.Errorf("mongo saslContinue: %w", err)
	}

	// Verify the server's ServerSignature; this is what makes SCRAM mutual auth.
	if _, err := conv.Step(string(serverFinal)); err != nil {
		return fmt.Errorf("mongo scram verify server signature: %w", err)
	}
	if !conv.Valid() {
		return fmt.Errorf("mongo scram conversation did not validate")
	}

	// Servers that ignore skipEmptyExchange end with done:false and expect a
	// final empty saslContinue; loop until the server reports the conversation
	// complete. The SCRAM exchange is already cryptographically finished here, so
	// these payloads are empty and their replies are not fed back into conv.
	for !done {
		reply, err = mongoCommandRoundTrip(conn, BSON{
			{Key: "saslContinue", Value: int32(1)},
			{Key: "conversationId", Value: convID},
			{Key: "payload", Value: BSONBinary{Subtype: BinarySubtypeGeneric, Data: []byte{}}},
			{Key: "$db", Value: authDB},
		})
		if err != nil {
			return fmt.Errorf("mongo saslContinue (finalize): %w", err)
		}
		if _, _, done, err = parseMongoSaslReply(reply); err != nil {
			return fmt.Errorf("mongo saslContinue (finalize): %w", err)
		}
	}
	return nil
}

// mongoProxyDriverVersion is the version dbpivot reports as the "driver" in its
// upstream handshake client metadata. It is cosmetic (it surfaces in the
// upstream's logs / db.currentOp) and need not track the dbpivot binary version.
const mongoProxyDriverVersion = "1.0"

// mongoUpstreamHandshake performs the mandatory MongoDB connection handshake on
// a fresh upstream connection: a single `hello` carrying client metadata, which
// a real mongod requires before it will accept any other command (saslStart
// included). db is the database the hello runs against (the auth database is a
// fine choice; hello ignores it). It errors unless the server replies ok:1.
func mongoUpstreamHandshake(conn net.Conn, db string) error {
	reply, err := mongoCommandRoundTrip(conn, BSON{
		{Key: "hello", Value: int32(1)},
		{Key: "client", Value: mongoUpstreamClientMetadata()},
		{Key: "$db", Value: db},
	})
	if err != nil {
		return err
	}
	if !mongoReplyOK(reply) {
		return mongoReplyError(reply)
	}
	return nil
}

// mongoUpstreamClientMetadata is the client metadata dbpivot presents to the
// upstream in its handshake hello. MongoDB records it (visible in server logs
// and db.currentOp) and accepts it only on the first hello of a connection. It
// identifies the proxy rather than impersonating the connecting client.
func mongoUpstreamClientMetadata() BSON {
	return BSON{
		{Key: "driver", Value: BSON{
			{Key: "name", Value: "dbpivot"},
			{Key: "version", Value: mongoProxyDriverVersion},
		}},
		{Key: "os", Value: BSON{
			{Key: "type", Value: runtime.GOOS},
		}},
	}
}

// mongoCommandRoundTrip sends cmd as a single-section OP_MSG command and reads
// the OP_MSG reply, returning the reply's body document. It is the upstream
// counterpart to the client-facing ReadMongoCommand/WriteMongoReply pair and is
// used both for the sasl exchange and (later) for an upstream hello probe.
func mongoCommandRoundTrip(conn net.Conn, cmd BSON) (BSON, error) {
	if err := WriteMongoMessage(conn, 0, 0, OpMsg, EncodeOpMsgBody(0, cmd)); err != nil {
		return nil, fmt.Errorf("write command: %w", err)
	}
	hdr, body, err := ReadMongoMessage(conn)
	if err != nil {
		return nil, fmt.Errorf("read reply: %w", err)
	}
	if hdr.OpCode != OpMsg {
		return nil, fmt.Errorf("expected OP_MSG reply, got opcode %d", hdr.OpCode)
	}
	_, doc, err := ParseOpMsg(body)
	if err != nil {
		return nil, fmt.Errorf("parse reply: %w", err)
	}
	return doc, nil
}

// parseMongoSaslReply extracts the fields of a saslStart/saslContinue reply:
// the conversationId (echoed back in the next request), the SCRAM payload bytes
// (the server's message for this step), and the done flag (whether the server
// considers the conversation finished). A reply with ok != 1 is surfaced as an
// error built from its errmsg/code.
func parseMongoSaslReply(doc BSON) (convID int32, payload []byte, done bool, err error) {
	if !mongoReplyOK(doc) {
		return 0, nil, false, mongoReplyError(doc)
	}
	if v, ok := doc.Lookup("conversationId"); ok {
		convID = mongoToInt32(v)
	}
	if v, ok := doc.Lookup("payload"); ok {
		if bin, ok := v.(BSONBinary); ok {
			payload = bin.Data
		} else {
			return 0, nil, false, fmt.Errorf("sasl reply payload is %T, want binary", v)
		}
	}
	if v, ok := doc.Lookup("done"); ok {
		if b, ok := v.(bool); ok {
			done = b
		}
	}
	return convID, payload, done, nil
}

// mongoReplyOK reports whether a command reply's `ok` field is 1. MongoDB may
// encode it as a double (the usual 1.0), an int32, or an int64, so all numeric
// forms are accepted.
func mongoReplyOK(doc BSON) bool {
	v, ok := doc.Lookup("ok")
	if !ok {
		return false
	}
	switch n := v.(type) {
	case float64:
		return n == 1
	case int32:
		return n == 1
	case int64:
		return n == 1
	case int:
		return n == 1
	default:
		return false
	}
}

// mongoReplyError builds an error from a failed (ok:0) command reply, using its
// errmsg and code fields when present.
func mongoReplyError(doc BSON) error {
	msg := "command failed"
	if s, ok := lookupBSONString(doc, "errmsg"); ok && s != "" {
		msg = s
	}
	if v, ok := doc.Lookup("code"); ok {
		if code := mongoToInt32(v); code != 0 {
			return fmt.Errorf("%s (code %d)", msg, code)
		}
	}
	return fmt.Errorf("%s", msg)
}

// mongoToInt32 coerces a BSON numeric value to int32, returning 0 for any other
// type. MongoDB returns conversationId and code as 32-bit ints but tolerating
// int64/double keeps the parser robust across server versions.
func mongoToInt32(v any) int32 {
	switch n := v.(type) {
	case int32:
		return n
	case int64:
		return int32(n)
	case int:
		return int32(n)
	case float64:
		return int32(n)
	default:
		return 0
	}
}
