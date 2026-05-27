package proxy

import (
	"context"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ykpythemind/dbpivot/internal/config"
)

// fakeMySQLUpstream is a TCP MySQL server that completes a native-password
// handshake (trust: any scramble accepted), records the client login it saw,
// and then echoes post-auth bytes. It advertises CLIENT_DEPRECATE_EOF so the
// proxy's capability-symmetry check matches a DEPRECATE_EOF-capable client.
type fakeMySQLUpstream struct {
	t    *testing.T
	ln   net.Listener
	port int
	last atomic.Value // *HandshakeResponse41
}

func newFakeMySQLUpstream(t *testing.T) *fakeMySQLUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fu := &fakeMySQLUpstream{t: t, ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	go fu.acceptLoop()
	return fu
}

func (fu *fakeMySQLUpstream) acceptLoop() {
	for {
		c, err := fu.ln.Accept()
		if err != nil {
			return
		}
		go fu.serve(c)
	}
}

func (fu *fakeMySQLUpstream) serve(c net.Conn) {
	defer c.Close()
	caps := uint32(ClientProtocol41 | ClientPluginAuth | ClientSecureConnection |
		ClientConnectWithDB | ClientDeprecateEOF)
	salt := make20Salt(7)
	hs := EncodeHandshakeV10("8.0.40-fake", 11, caps, DefaultCharsetUTF8MB4, 0x0002, MySQLNativePassword, salt)
	if _, err := WritePacket(c, 0, hs); err != nil {
		return
	}
	rseq, payload, err := ReadPacket(c)
	if err != nil {
		return
	}
	resp, err := ParseHandshakeResponse41(payload)
	if err != nil {
		return
	}
	fu.last.Store(resp)
	if _, err := WritePacket(c, rseq+1, EncodeOKPacket(0, 0, 0x0002, 0)); err != nil {
		return
	}
	// Echo post-auth (command-phase) bytes back to the client.
	_, _ = io.Copy(c, c)
}

func (fu *fakeMySQLUpstream) lastLogin() *HandshakeResponse41 {
	v, _ := fu.last.Load().(*HandshakeResponse41)
	return v
}

func (fu *fakeMySQLUpstream) close() { fu.ln.Close() }

// mysqlClientLogin drives the client side of the proxy greeting and returns the
// concluding packet payload (OK or ERR) plus the live connection.
func mysqlClientLogin(t *testing.T, addr, user, password, database string) (net.Conn, []byte) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))

	_, greeting, err := ReadPacket(c)
	if err != nil {
		c.Close()
		t.Fatalf("read greeting: %v", err)
	}
	hs, err := ParseHandshakeV10(greeting)
	if err != nil {
		c.Close()
		t.Fatalf("parse greeting: %v", err)
	}
	caps := uint32(ClientProtocol41 | ClientSecureConnection | ClientPluginAuth | ClientDeprecateEOF)
	if database != "" {
		caps |= ClientConnectWithDB
	}
	authResp := ScrambleNativePassword(password, hs.AuthPluginData)
	resp := EncodeHandshakeResponse41(caps, DefaultCharsetUTF8MB4, user, authResp, database, MySQLNativePassword)
	if _, err := WritePacket(c, 1, resp); err != nil {
		c.Close()
		t.Fatalf("write handshake response: %v", err)
	}
	_, concluding, err := ReadPacket(c)
	if err != nil {
		c.Close()
		t.Fatalf("read concluding packet: %v", err)
	}
	return c, concluding
}

func mysqlCfg(t *testing.T, target config.Target) *config.Config {
	return &config.Config{
		Port: freePort(t),
		Databases: []config.Database{
			{
				Adapter:     config.AdapterMySQL,
				VirtualName: "appdb",
				Targets:     []config.Target{target},
			},
		},
	}
}

func TestServerMySQL_RouteAndRewriteDatabase(t *testing.T) {
	up := newFakeMySQLUpstream(t)
	defer up.close()

	cfg := mysqlCfg(t, config.Target{
		Name: "local", Host: "127.0.0.1", Port: up.port,
		User: "alice", Password: "secret", Database: "app_real",
	})
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, concluding := mysqlClientLogin(t, s.Addr(), "anyone", "whatever", "appdb")
	defer c.Close()
	if !IsOKPacket(concluding) {
		t.Fatalf("expected OK, got header 0x%02x (%s)", firstByte(concluding), ParseERRPacket(concluding))
	}

	time.Sleep(50 * time.Millisecond)
	login := up.lastLogin()
	if login == nil {
		t.Fatal("upstream never received a login")
	}
	if login.Username != "alice" {
		t.Errorf("upstream user = %q, want alice", login.Username)
	}
	if login.Database != "app_real" {
		t.Errorf("upstream database = %q, want app_real", login.Database)
	}
}

func TestServerMySQL_EmptyDatabasePassthrough(t *testing.T) {
	up := newFakeMySQLUpstream(t)
	defer up.close()

	cfg := mysqlCfg(t, config.Target{
		Name: "local", Host: "127.0.0.1", Port: up.port, User: "alice", Password: "secret",
	})
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, concluding := mysqlClientLogin(t, s.Addr(), "anyone", "", "appdb")
	defer c.Close()
	if !IsOKPacket(concluding) {
		t.Fatalf("expected OK, got %s", ParseERRPacket(concluding))
	}

	time.Sleep(50 * time.Millisecond)
	if login := up.lastLogin(); login == nil || login.Database != "appdb" {
		t.Errorf("upstream database = %v, want passthrough \"appdb\"", login)
	}
}

func TestServerMySQL_UnknownDatabaseReturnsErr(t *testing.T) {
	cfg := mysqlCfg(t, config.Target{
		Name: "local", Host: "127.0.0.1", Port: 1, User: "u", Password: "p", Database: "x",
	})
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, concluding := mysqlClientLogin(t, s.Addr(), "u", "p", "missing")
	defer c.Close()
	if !IsErrPacket(concluding) {
		t.Fatalf("expected ERR, got header 0x%02x", firstByte(concluding))
	}
}

func TestServerMySQL_BidiPipeAfterAuth(t *testing.T) {
	up := newFakeMySQLUpstream(t)
	defer up.close()

	cfg := mysqlCfg(t, config.Target{
		Name: "local", Host: "127.0.0.1", Port: up.port,
		User: "alice", Password: "secret", Database: "app_real",
	})
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, concluding := mysqlClientLogin(t, s.Addr(), "anyone", "pw", "appdb")
	defer c.Close()
	if !IsOKPacket(concluding) {
		t.Fatalf("expected OK, got %s", ParseERRPacket(concluding))
	}

	msg := []byte("SELECT 1\n")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(msg) {
		t.Errorf("echo = %q, want %q", buf, msg)
	}
}
