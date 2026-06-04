package proxy

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ykpythemind/dbpivot/internal/config"
)

// fakeMongod listens on 127.0.0.1:0 and, for each accepted connection, completes
// the SCRAM-SHA-256 server side, then serves the forwarded command phase: it
// reads each command, records the $db and command name it saw, and replies ok:1
// echoing the command name. It is the MongoDB analog of fakeUpstream.
type fakeMongod struct {
	ln       net.Listener
	port     int
	user     string
	password string
	gotDB    atomic.Value // string
	gotCmd   atomic.Value // string (command name)
}

func newFakeMongod(t *testing.T, user, password string) *fakeMongod {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fm := &fakeMongod{ln: ln, port: ln.Addr().(*net.TCPAddr).Port, user: user, password: password}
	go fm.acceptLoop()
	return fm
}

func (fm *fakeMongod) acceptLoop() {
	for {
		c, err := fm.ln.Accept()
		if err != nil {
			return
		}
		go fm.serve(c)
	}
}

func (fm *fakeMongod) serve(c net.Conn) {
	defer c.Close()
	if err := serveFakeMongoSCRAM(c, fm.user, fm.password, true); err != nil {
		return
	}
	for {
		hdr, body, err := ReadMongoMessage(c)
		if err != nil {
			return
		}
		cmd, err := decodeMongoCommand(hdr, body)
		if err != nil {
			return
		}
		fm.gotDB.Store(cmd.DB)
		fm.gotCmd.Store(cmd.CommandName())
		_ = WriteMongoReply(c, hdr.OpCode, hdr.RequestID, BSON{
			{Key: "ok", Value: 1.0},
			{Key: "echo", Value: cmd.CommandName()},
		})
	}
}

func (fm *fakeMongod) close() { fm.ln.Close() }

// sendMongoCmd writes a command as a single-section OP_MSG with the given
// request id.
func sendMongoCmd(t *testing.T, c net.Conn, reqID int32, doc BSON) {
	t.Helper()
	if err := WriteMongoMessage(c, reqID, 0, OpMsg, EncodeOpMsgBody(0, doc)); err != nil {
		t.Fatalf("send mongo command: %v", err)
	}
}

// readMongoReply reads one OP_MSG reply, returning its body document and the
// ResponseTo (which the driver correlates against the request id).
func readMongoReply(t *testing.T, c net.Conn) (BSON, int32) {
	t.Helper()
	hdr, body, err := ReadMongoMessage(c)
	if err != nil {
		t.Fatalf("read mongo reply: %v", err)
	}
	if hdr.OpCode != OpMsg {
		t.Fatalf("reply opcode = %d, want OP_MSG", hdr.OpCode)
	}
	_, doc, err := ParseOpMsg(body)
	if err != nil {
		t.Fatalf("parse reply: %v", err)
	}
	return doc, hdr.ResponseTo
}

func mongoTestConfig(t *testing.T, up *fakeMongod) *config.Config {
	return &config.Config{
		ListenPorts: map[string]int{config.AdapterMongo: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterMongo,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: up.port, User: "alice", Password: "secret", Database: "app_real"},
				},
			},
		},
	}
}

// TestServer_Mongo_HelloRouteAndForward drives the full client→proxy→upstream
// path: the proxy answers an auth-disabled hello locally, then routes the first
// command naming a configured database to the upstream (SCRAM auth + verbatim
// forward) and pipes the upstream reply straight back.
func TestServer_Mongo_HelloRouteAndForward(t *testing.T) {
	up := newFakeMongod(t, "alice", "secret")
	defer up.close()

	s := startServer(t, mongoTestConfig(t, up))
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterMongo))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// 1. hello on admin -> answered locally with an auth-disabled reply.
	sendMongoCmd(t, c, 1, BSON{{Key: "hello", Value: int32(1)}, {Key: "$db", Value: "admin"}})
	hello, respTo := readMongoReply(t, c)
	if respTo != 1 {
		t.Errorf("hello ResponseTo = %d, want 1", respTo)
	}
	if v, _ := hello.Lookup("isWritablePrimary"); v != true {
		t.Errorf("hello isWritablePrimary = %v, want true", v)
	}
	if _, ok := hello.Lookup("saslSupportedMechs"); ok {
		t.Error("hello must advertise auth disabled (no saslSupportedMechs)")
	}
	if !mongoReplyOK(hello) {
		t.Error("hello reply not ok:1")
	}

	// 2. command on the configured db -> routed, upstream authed, forwarded.
	sendMongoCmd(t, c, 2, BSON{{Key: "ping", Value: int32(1)}, {Key: "$db", Value: "appdb"}})
	reply, respTo := readMongoReply(t, c)
	if respTo != 2 {
		t.Errorf("command ResponseTo = %d, want 2", respTo)
	}
	if !mongoReplyOK(reply) {
		t.Errorf("forwarded command reply not ok:1: %+v", reply)
	}
	if v, _ := reply.Lookup("echo"); v != "ping" {
		t.Errorf("echo = %v, want ping", v)
	}

	if got, _ := up.gotDB.Load().(string); got != "appdb" {
		t.Errorf("upstream received $db = %q, want appdb", got)
	}
	if got, _ := up.gotCmd.Load().(string); got != "ping" {
		t.Errorf("upstream received command = %q, want ping", got)
	}
}

// TestServer_Mongo_AdminChatterAnsweredLocally verifies the pre-routing admin
// commands real drivers/mongosh send on $db:admin (ping, buildInfo, ...) are
// answered locally with ok:1 — not forwarded and not ok:0-errored — so the
// handshake completes before any command names a configured database.
func TestServer_Mongo_AdminChatterAnsweredLocally(t *testing.T) {
	up := newFakeMongod(t, "alice", "secret")
	defer up.close()

	s := startServer(t, mongoTestConfig(t, up))
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterMongo))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// hello first, then a burst of admin probes on admin — all local, ok:1.
	sendMongoCmd(t, c, 1, BSON{{Key: "hello", Value: int32(1)}, {Key: "$db", Value: "admin"}})
	readMongoReply(t, c)

	var reqID int32 = 2
	for _, name := range []string{"ping", "buildInfo", "whatsmyuri", "getLog", "connectionStatus"} {
		sendMongoCmd(t, c, reqID, BSON{{Key: name, Value: int32(1)}, {Key: "$db", Value: "admin"}})
		reply, respTo := readMongoReply(t, c)
		if respTo != reqID {
			t.Errorf("%s ResponseTo = %d, want %d", name, respTo, reqID)
		}
		if !mongoReplyOK(reply) {
			t.Errorf("%s not answered ok:1 locally: %+v", name, reply)
		}
		reqID++
	}

	// None of the admin chatter should have reached the upstream.
	if got, _ := up.gotCmd.Load().(string); got != "" {
		t.Errorf("upstream saw command %q, want none (admin chatter must stay local)", got)
	}
}

// TestServer_Mongo_UnknownDatabaseErrors verifies a command naming an
// unconfigured database gets an ok:0 error and the connection stays open so a
// subsequent hello is still served.
func TestServer_Mongo_UnknownDatabaseErrors(t *testing.T) {
	up := newFakeMongod(t, "alice", "secret")
	defer up.close()

	s := startServer(t, mongoTestConfig(t, up))
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterMongo))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	sendMongoCmd(t, c, 1, BSON{{Key: "find", Value: "coll"}, {Key: "$db", Value: "nope"}})
	reply, respTo := readMongoReply(t, c)
	if respTo != 1 {
		t.Errorf("ResponseTo = %d, want 1", respTo)
	}
	if mongoReplyOK(reply) {
		t.Errorf("expected ok:0 for unconfigured db, got %+v", reply)
	}
	if msg, _ := lookupBSONString(reply, "errmsg"); !strings.Contains(msg, "nope") {
		t.Errorf("errmsg should mention the db, got %q", msg)
	}

	// The connection is still usable: a hello afterwards is answered locally.
	sendMongoCmd(t, c, 2, BSON{{Key: "hello", Value: int32(1)}, {Key: "$db", Value: "admin"}})
	hello, respTo := readMongoReply(t, c)
	if respTo != 2 || !mongoReplyOK(hello) {
		t.Errorf("hello after error not served: respTo=%d ok=%v", respTo, mongoReplyOK(hello))
	}
}
