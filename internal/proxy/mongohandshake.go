package proxy

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// MongoDB client-facing handshake (server side of the client→proxy leg).
//
// This is the MongoDB analog of mysqlclient.go's GreetClientMySQL: the logic
// the proxy uses to answer a connecting client. Like PostgreSQL, MongoDB is
// client-first — the driver opens the conversation by sending a `hello` (modern)
// or `isMaster` (legacy) command and expects a reply before doing anything else.
//
// Two things make the MongoDB handshake different from PG/MySQL and shape the
// design here:
//
//   - Auth model. The client→proxy leg cannot offer SCRAM "trust" auth (the
//     server side of SCRAM must derive a ServerSignature from the password the
//     client also knows, so "accept any password" is impossible). Instead the
//     proxy advertises an auth-disabled standalone: BuildHelloReply omits
//     saslSupportedMechs, so a client connecting WITHOUT URI credentials skips
//     authentication entirely and proceeds straight to commands. The proxy then
//     performs SCRAM only on the upstream leg, with the configured credentials.
//
//   - Routing. MongoDB carries the target database per-command in $db, and the
//     very first hello is always sent against the `admin` database, so the
//     handshake itself reveals nothing about which configured virtual database
//     the client wants. The proxy therefore answers the handshake on its own
//     and defers routing to the first command that names a real database.
//
// These functions are pure helpers over io.Reader/io.Writer (no net.Conn, no
// server state), so they can be unit-tested in isolation and composed into the
// connection dispatcher later, mirroring mongowire.go / bson.go.

// Limits the proxy advertises in its hello reply. These mirror a standalone
// mongod's defaults. Because dbpivot pipes the command phase straight through
// to the upstream after the handshake, they only shape how the driver frames
// requests during and just after the handshake.
const (
	mongoMaxBSONObjectSize    = 16 * 1024 * 1024 // 16 MiB
	mongoMaxMessageSizeBytes  = 48 * 1000 * 1000 // 48 MB (mongod default)
	mongoMaxWriteBatchSize    = 100000
	mongoLogicalSessionTTLMin = 30
)

// Wire-version range advertised to clients. minWireVersion 0 keeps very old
// drivers from bailing out; maxWireVersion 17 corresponds to MongoDB 6.0, new
// enough for every current driver/mongosh to use OP_MSG yet old enough to stay
// compatible with the upstreams dbpivot is likely fronting. Because the proxy
// pipes raw after the handshake, this is the protocol level both legs must
// agree on; it can be made configurable later.
const (
	mongoMinWireVersion = 0
	mongoMaxWireVersion = 17
)

// IsHelloCommand reports whether doc is a hello/isMaster handshake probe — i.e.
// its first element's key (the command name) is one of hello / isMaster /
// ismaster. The comparison is case-insensitive because drivers send both
// "isMaster" and "ismaster".
func IsHelloCommand(doc BSON) bool {
	if len(doc) == 0 {
		return false
	}
	switch strings.ToLower(doc[0].Key) {
	case "hello", "ismaster":
		return true
	default:
		return false
	}
}

// BuildHelloReply constructs the synthetic hello reply the proxy returns to a
// connecting client. It advertises a writable standalone with authentication
// disabled (no saslSupportedMechs), so a client connecting without URI
// credentials proceeds straight to issuing commands; the proxy performs auth
// only on the upstream leg. connectionID is echoed back as connectionId and
// now sets localTime.
func BuildHelloReply(connectionID int32, now time.Time) BSON {
	return BSON{
		{Key: "isWritablePrimary", Value: true},
		{Key: "ismaster", Value: true}, // legacy alias for pre-`hello` drivers
		// helloOk advertises that the server understands the `hello` command, so
		// drivers may keep using it instead of the legacy isMaster.
		{Key: "helloOk", Value: true},
		{Key: "maxBsonObjectSize", Value: int32(mongoMaxBSONObjectSize)},
		{Key: "maxMessageSizeBytes", Value: int32(mongoMaxMessageSizeBytes)},
		{Key: "maxWriteBatchSize", Value: int32(mongoMaxWriteBatchSize)},
		{Key: "localTime", Value: BSONDateTime(now.UnixMilli())},
		{Key: "logicalSessionTimeoutMinutes", Value: int32(mongoLogicalSessionTTLMin)},
		{Key: "connectionId", Value: connectionID},
		{Key: "minWireVersion", Value: int32(mongoMinWireVersion)},
		{Key: "maxWireVersion", Value: int32(mongoMaxWireVersion)},
		{Key: "readOnly", Value: false},
		{Key: "ok", Value: 1.0},
	}
}

// MongoCommand is one decoded client request: the command document, the target
// database the client named (from $db on OP_MSG, or the namespace prefix on a
// legacy OP_QUERY), and the framing details needed to send a matching reply.
type MongoCommand struct {
	Doc       BSON
	DB        string
	OpCode    int32 // OpMsg or OpQuery
	RequestID int32 // the request's header RequestID; a reply's ResponseTo
}

// CommandName returns the command name (the first element's key), or "" for an
// empty document.
func (c MongoCommand) CommandName() string {
	if len(c.Doc) == 0 {
		return ""
	}
	return c.Doc[0].Key
}

// ReadMongoCommand reads one client message and decodes the command it carries.
// It understands OP_MSG (the modern transport) and OP_QUERY (the legacy probe a
// driver sends on "<db>.$cmd" before it knows OP_MSG is supported). Any other
// opcode is rejected — the proxy only ever sees commands during the phase this
// helper drives.
func ReadMongoCommand(r io.Reader) (MongoCommand, error) {
	hdr, body, err := ReadMongoMessage(r)
	if err != nil {
		return MongoCommand{}, err
	}
	switch hdr.OpCode {
	case OpMsg:
		_, doc, err := ParseOpMsg(body)
		if err != nil {
			return MongoCommand{}, err
		}
		db, _ := lookupBSONString(doc, "$db")
		return MongoCommand{Doc: doc, DB: db, OpCode: OpMsg, RequestID: hdr.RequestID}, nil
	case OpQuery:
		_, fullCollection, _, _, query, err := ParseOpQuery(body)
		if err != nil {
			return MongoCommand{}, err
		}
		db, _, _ := strings.Cut(fullCollection, ".")
		return MongoCommand{Doc: query, DB: db, OpCode: OpQuery, RequestID: hdr.RequestID}, nil
	default:
		return MongoCommand{}, fmt.Errorf("mongo: unexpected opcode %d during handshake", hdr.OpCode)
	}
}

// WriteMongoReply writes a command reply framed to match the request's opcode:
// an OP_MSG request gets an OP_MSG reply (single kind-0 body section), while a
// legacy OP_QUERY probe gets an OP_REPLY carrying the document. responseTo must
// be the request's RequestID so the driver can correlate the reply. The reply's
// own RequestID is left 0 (drivers correlate on ResponseTo, not the reply's id).
func WriteMongoReply(w io.Writer, reqOpCode, responseTo int32, doc BSON) error {
	switch reqOpCode {
	case OpMsg:
		return WriteMongoMessage(w, 0, responseTo, OpMsg, EncodeOpMsgBody(0, doc))
	case OpQuery:
		return WriteMongoMessage(w, 0, responseTo, OpReply, EncodeOpReplyBody(0, 0, 0, []BSON{doc}))
	default:
		return fmt.Errorf("mongo: cannot reply to opcode %d", reqOpCode)
	}
}

// lookupBSONString returns the string value for key in doc, reporting whether
// it was present and actually a string.
func lookupBSONString(doc BSON, key string) (string, bool) {
	v, ok := doc.Lookup(key)
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
