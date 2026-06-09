package proxy

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/xdg-go/scram"
)

// fakeMongoSCRAM plays the server side of a MongoDB SCRAM-SHA-256 exchange over
// OP_MSG on conn: read saslStart, reply server-first; read saslContinue, reply
// server-final. When skipEmptyExchange is false it marks the server-final
// done:false and serves one extra empty saslContinue (the pre-4.4 behavior).
// Errors are reported via errCh; conn is closed when the exchange ends.
func fakeMongoSCRAM(conn net.Conn, user, password string, skipEmptyExchange bool, errCh chan<- error) {
	defer conn.Close()
	if _, err := serveFakeMongoSCRAM(conn, user, password, skipEmptyExchange); err != nil {
		errCh <- err
	}
}

// serveFakeMongoSCRAM plays the server side of one SCRAM-SHA-256 exchange over
// OP_MSG on conn and returns when the conversation completes, leaving conn open
// for any command phase that follows. It does not close conn — callers that
// only need the auth (fakeMongoSCRAM) close it themselves, while the server-level
// fake mongod keeps the connection to serve the forwarded command. It returns
// the authentication database ($db) the saslStart was sent against so callers
// can assert how the upstream auth was scoped.
func serveFakeMongoSCRAM(conn net.Conn, user, password string, skipEmptyExchange bool) (authDB string, err error) {
	// Build the credential lookup the SCRAM server needs from a client computing
	// the stored keys for a fixed salt/iteration count.
	kf := scram.KeyFactors{Salt: "Zm9vYmFyc2FsdA==", Iters: 4096}
	credClient, err := scram.SHA256.NewClient(user, password, "")
	if err != nil {
		return "", fmt.Errorf("server cred client: %w", err)
	}
	stored, err := credClient.GetStoredCredentialsWithError(kf)
	if err != nil {
		return "", fmt.Errorf("server stored creds: %w", err)
	}
	srv, err := scram.SHA256.NewServer(func(u string) (scram.StoredCredentials, error) {
		if u != user {
			return scram.StoredCredentials{}, fmt.Errorf("unknown user %q", u)
		}
		return stored, nil
	})
	if err != nil {
		return "", fmt.Errorf("server: %w", err)
	}
	sconv := srv.NewConversation()
	const convID = int32(1)

	readPayload := func() (int32, []byte, BSON, error) {
		hdr, body, err := ReadMongoMessage(conn)
		if err != nil {
			return 0, nil, nil, err
		}
		_, doc, err := ParseOpMsg(body)
		if err != nil {
			return 0, nil, nil, err
		}
		v, _ := doc.Lookup("payload")
		bin, _ := v.(BSONBinary)
		return hdr.RequestID, bin.Data, doc, nil
	}
	writeReply := func(responseTo int32, payload []byte, done bool) error {
		return WriteMongoMessage(conn, 0, responseTo, OpMsg, EncodeOpMsgBody(0, BSON{
			{Key: "conversationId", Value: convID},
			{Key: "done", Value: done},
			{Key: "payload", Value: BSONBinary{Subtype: BinarySubtypeGeneric, Data: payload}},
			{Key: "ok", Value: 1.0},
		}))
	}

	// The proxy now performs the mandatory connection handshake (a hello)
	// before saslStart, mirroring what a real mongod requires; consume it and
	// answer with a standalone hello reply so auth can proceed.
	helloID, _, helloDoc, err := readPayload()
	if err != nil {
		return "", fmt.Errorf("read handshake hello: %w", err)
	}
	if !IsHelloCommand(helloDoc) {
		return "", fmt.Errorf("expected handshake hello first, got command %q", MongoCommand{Doc: helloDoc}.CommandName())
	}
	if err := WriteMongoMessage(conn, 0, helloID, OpMsg, EncodeOpMsgBody(0, BuildHelloReply(1, time.Now()))); err != nil {
		return "", fmt.Errorf("write handshake hello reply: %w", err)
	}

	// saslStart -> server-first. The saslStart's $db is the upstream auth
	// database the proxy chose (default "admin" or the configured auth_source).
	reqID, clientFirst, startDoc, err := readPayload()
	if err != nil {
		return "", fmt.Errorf("read saslStart: %w", err)
	}
	authDB, _ = lookupBSONString(startDoc, "$db")
	serverFirst, err := sconv.Step(string(clientFirst))
	if err != nil {
		return authDB, fmt.Errorf("server step1: %w", err)
	}
	if err := writeReply(reqID, []byte(serverFirst), false); err != nil {
		return authDB, fmt.Errorf("write server-first: %w", err)
	}

	// saslContinue -> server-final.
	reqID, clientFinal, _, err := readPayload()
	if err != nil {
		return authDB, fmt.Errorf("read saslContinue: %w", err)
	}
	serverFinal, err := sconv.Step(string(clientFinal))
	if err != nil {
		return authDB, fmt.Errorf("server step2: %w", err)
	}
	if err := writeReply(reqID, []byte(serverFinal), skipEmptyExchange); err != nil {
		return authDB, fmt.Errorf("write server-final: %w", err)
	}

	// Pre-4.4 behavior: one extra empty saslContinue concludes the exchange.
	if !skipEmptyExchange {
		reqID, _, _, err := readPayload()
		if err != nil {
			return authDB, fmt.Errorf("read finalize saslContinue: %w", err)
		}
		if err := writeReply(reqID, nil, true); err != nil {
			return authDB, fmt.Errorf("write finalize: %w", err)
		}
	}
	return authDB, nil
}

func TestAuthenticateUpstreamMongo_Success(t *testing.T) {
	for _, skip := range []bool{true, false} {
		name := "skipEmptyExchange"
		if !skip {
			name = "emptyExchangeLoop"
		}
		t.Run(name, func(t *testing.T) {
			client, server := net.Pipe()
			errCh := make(chan error, 1)
			go func() {
				fakeMongoSCRAM(server, "alice", "s3cret", skip, errCh)
				close(errCh)
			}()

			done := make(chan error, 1)
			go func() { done <- AuthenticateUpstreamMongo(client, "alice", "s3cret", "admin") }()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("AuthenticateUpstreamMongo: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out")
			}
			for err := range errCh {
				if err != nil {
					t.Fatalf("fake server: %v", err)
				}
			}
			client.Close()
		})
	}
}

func TestAuthenticateUpstreamMongo_WrongPassword(t *testing.T) {
	client, server := net.Pipe()
	errCh := make(chan error, 4)
	// Server stores creds for the real password; the client offers a wrong one,
	// so the SCRAM proof fails server-side and our client must surface an error.
	go func() {
		fakeMongoSCRAM(server, "alice", "correct-horse", true, errCh)
		close(errCh)
	}()

	done := make(chan error, 1)
	go func() { done <- AuthenticateUpstreamMongo(client, "alice", "wrong-password", "admin") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected authentication error, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	client.Close()
}

// TestAuthenticateUpstreamMongo_CommandError verifies that an ok:0 reply (e.g.
// "Authentication failed") is surfaced with its errmsg/code via mongoReplyError.
func TestAuthenticateUpstreamMongo_CommandError(t *testing.T) {
	client, server := net.Pipe()
	go func() {
		defer server.Close()
		// First answer the mandatory handshake hello with an ok:1 reply.
		hhdr, _, err := ReadMongoMessage(server)
		if err != nil {
			return
		}
		if err := WriteMongoMessage(server, 0, hhdr.RequestID, OpMsg, EncodeOpMsgBody(0, BuildHelloReply(1, time.Now()))); err != nil {
			return
		}
		// Then read the saslStart and reject it with an ok:0 error reply.
		hdr, _, err := ReadMongoMessage(server)
		if err != nil {
			return
		}
		_ = WriteMongoMessage(server, 0, hdr.RequestID, OpMsg, EncodeOpMsgBody(0, BSON{
			{Key: "ok", Value: 0.0},
			{Key: "errmsg", Value: "Authentication failed."},
			{Key: "code", Value: int32(18)},
			{Key: "codeName", Value: "AuthenticationFailed"},
		}))
	}()

	done := make(chan error, 1)
	go func() { done <- AuthenticateUpstreamMongo(client, "alice", "s3cret", "admin") }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected command error, got nil")
		}
		if !strings.Contains(err.Error(), "Authentication failed") || !strings.Contains(err.Error(), "18") {
			t.Fatalf("error should carry errmsg and code, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out")
	}
	client.Close()
}

func TestParseMongoSaslReply(t *testing.T) {
	t.Run("ok reply", func(t *testing.T) {
		convID, payload, done, err := parseMongoSaslReply(BSON{
			{Key: "conversationId", Value: int32(7)},
			{Key: "done", Value: true},
			{Key: "payload", Value: BSONBinary{Subtype: BinarySubtypeGeneric, Data: []byte("v=abc")}},
			{Key: "ok", Value: 1.0},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if convID != 7 || !done || string(payload) != "v=abc" {
			t.Fatalf("got convID=%d done=%v payload=%q", convID, done, payload)
		}
	})
	t.Run("error reply", func(t *testing.T) {
		_, _, _, err := parseMongoSaslReply(BSON{
			{Key: "ok", Value: 0.0},
			{Key: "errmsg", Value: "nope"},
		})
		if err == nil || !strings.Contains(err.Error(), "nope") {
			t.Fatalf("want errmsg-bearing error, got %v", err)
		}
	})
	t.Run("missing ok is failure", func(t *testing.T) {
		if _, _, _, err := parseMongoSaslReply(BSON{{Key: "x", Value: int32(1)}}); err == nil {
			t.Fatal("reply without ok should be treated as failure")
		}
	})
}

func TestMongoReplyOK(t *testing.T) {
	cases := []struct {
		val  any
		want bool
	}{
		{1.0, true},
		{int32(1), true},
		{int64(1), true},
		{int(1), true},
		{0.0, false},
		{int32(0), false},
		{"1", false},
	}
	for _, c := range cases {
		if got := mongoReplyOK(BSON{{Key: "ok", Value: c.val}}); got != c.want {
			t.Errorf("mongoReplyOK(ok=%v(%T)) = %v, want %v", c.val, c.val, got, c.want)
		}
	}
	if mongoReplyOK(BSON{}) {
		t.Error("mongoReplyOK with no ok field should be false")
	}
}
