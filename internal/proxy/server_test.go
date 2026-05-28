package proxy

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ykpythemind/dbpivot/internal/config"
)

// fakeUpstream listens on 127.0.0.1:0 and, for each accepted connection,
// completes a SCRAM-SHA-256 server handshake with the configured user and
// password, then captures the client-sent StartupMessage params and echoes
// any post-auth bytes back to the client.
type fakeUpstream struct {
	t        *testing.T
	ln       net.Listener
	port     int
	user     string
	password string
	last     atomic.Value // []KV
}

func newFakeUpstream(t *testing.T, user, password string) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	fu := &fakeUpstream{t: t, ln: ln, port: addr.Port, user: user, password: password}
	go fu.acceptLoop()
	return fu
}

func (fu *fakeUpstream) acceptLoop() {
	for {
		c, err := fu.ln.Accept()
		if err != nil {
			return
		}
		go fu.serve(c)
	}
}

func (fu *fakeUpstream) serve(c net.Conn) {
	defer c.Close()

	// Read the StartupMessage frame.
	var lb [4]byte
	if _, err := io.ReadFull(c, lb[:]); err != nil {
		return
	}
	msgLen := binary.BigEndian.Uint32(lb[:])
	body := make([]byte, msgLen-4)
	if _, err := io.ReadFull(c, body); err != nil {
		return
	}
	if binary.BigEndian.Uint32(body[:4]) != ProtocolV3 {
		return
	}
	params, err := ParseStartupBody(body[4:])
	if err != nil {
		return
	}
	fu.last.Store(params)

	if err := runFakeSCRAMServer(c, fu.user, fu.password); err != nil {
		return
	}

	// Echo any remaining bytes (post-auth).
	_, _ = io.Copy(c, c)
}

func (fu *fakeUpstream) lastParams() []KV {
	v, _ := fu.last.Load().([]KV)
	return v
}

func (fu *fakeUpstream) close() { fu.ln.Close() }

func startServer(t *testing.T, cfg *config.Config) *Server {
	t.Helper()
	s, err := New(cfg, "local", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	go s.Start()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		ready := true
		for _, addr := range s.Addrs() {
			c, err := net.Dial("tcp", addr)
			if err != nil {
				ready = false
				break
			}
			c.Close()
		}
		if ready {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("server did not start on %v", s.Addrs())
	return nil
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestServer_RouteByDbnameAndRewriteDatabase(t *testing.T) {
	up := newFakeUpstream(t, "alice", "secret")
	defer up.close()

	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: up.port, User: "alice", Password: "secret", Database: "app_real"},
				},
			},
		},
	}
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterPostgres))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	clientParams := []KV{
		{K: "user", V: "anyone"},
		{K: "database", V: "appdb"},
		{K: "application_name", V: "test"},
	}
	if err := SendStartupMessage(c, clientParams); err != nil {
		t.Fatal(err)
	}

	// Read the AuthenticationOk we expect the proxy to deliver.
	typ, body, err := ReadMessage(c)
	if err != nil {
		t.Fatalf("read AuthOk: %v", err)
	}
	if typ != 'R' {
		t.Errorf("typ = %c", typ)
	}
	code, _, _ := ParseAuthenticationMessage(body)
	if code != AuthOk {
		t.Errorf("code = %d", code)
	}

	// Allow goroutines to settle, then inspect what upstream actually got.
	time.Sleep(50 * time.Millisecond)
	got := up.lastParams()
	if got == nil {
		t.Fatal("upstream never received params")
	}
	if u := LookupParam(got, "user"); u != "alice" {
		t.Errorf("upstream user = %q, want alice", u)
	}
	if d := LookupParam(got, "database"); d != "app_real" {
		t.Errorf("upstream database = %q, want app_real", d)
	}
	if a := LookupParam(got, "application_name"); a != "test" {
		t.Errorf("upstream application_name = %q, want test", a)
	}
}

func TestServer_SSLRequestThenStartup(t *testing.T) {
	up := newFakeUpstream(t, "alice", "secret")
	defer up.close()

	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: up.port, User: "alice", Password: "secret", Database: "app_real"},
				},
			},
		},
	}
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterPostgres))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := SendSSLRequest(c); err != nil {
		t.Fatal(err)
	}
	var nbuf [1]byte
	if _, err := io.ReadFull(c, nbuf[:]); err != nil {
		t.Fatalf("read N: %v", err)
	}
	if nbuf[0] != 'N' {
		t.Errorf("expected 'N', got %c", nbuf[0])
	}

	if err := SendStartupMessage(c, []KV{{K: "user", V: "anyone"}, {K: "database", V: "appdb"}}); err != nil {
		t.Fatal(err)
	}
	typ, _, err := ReadMessage(c)
	if err != nil {
		t.Fatalf("read AuthOk: %v", err)
	}
	if typ != 'R' {
		t.Errorf("typ = %c", typ)
	}
}

func TestServer_UnknownDatabaseReturnsErrorResponse(t *testing.T) {
	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 1, User: "u", Password: "p", Database: "x"},
				},
			},
		},
	}
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterPostgres))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := SendStartupMessage(c, []KV{{K: "user", V: "u"}, {K: "database", V: "missing"}}); err != nil {
		t.Fatal(err)
	}
	typ, body, err := ReadMessage(c)
	if err != nil {
		t.Fatalf("read ErrorResponse: %v", err)
	}
	if typ != 'E' {
		t.Errorf("typ = %c, want E", typ)
	}
	if !strings.Contains(string(body), `database "missing"`) {
		t.Errorf("body missing database name: %q", body)
	}
}

func TestServer_CancelRequestDropped(t *testing.T) {
	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 1, User: "u", Password: "p", Database: "x"},
				},
			},
		},
	}
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterPostgres))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := SendCancelRequest(c, 42, 99); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	n, err := c.Read(buf[:])
	if err == nil && n > 0 {
		t.Errorf("expected no data from server (got %d byte)", n)
	}
}

func TestServer_EmptyDatabasePassthrough(t *testing.T) {
	up := newFakeUpstream(t, "alice", "secret")
	defer up.close()

	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: up.port, User: "alice", Password: "secret"},
				},
			},
		},
	}
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterPostgres))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := SendStartupMessage(c, []KV{{K: "user", V: "anyone"}, {K: "database", V: "appdb"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMessage(c); err != nil {
		t.Fatalf("read AuthOk: %v", err)
	}

	time.Sleep(50 * time.Millisecond)
	got := up.lastParams()
	if d := LookupParam(got, "database"); d != "appdb" {
		t.Errorf("upstream database = %q, want passthrough \"appdb\"", d)
	}
}

func TestServer_BidiPipeAfterAuth(t *testing.T) {
	up := newFakeUpstream(t, "alice", "secret")
	defer up.close()

	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: up.port, User: "alice", Password: "secret", Database: "app_real"},
				},
			},
		},
	}
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterPostgres))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := SendStartupMessage(c, []KV{{K: "user", V: "x"}, {K: "database", V: "appdb"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMessage(c); err != nil {
		t.Fatalf("read AuthOk: %v", err)
	}

	msg := []byte("hello\n")
	if _, err := c.Write(msg); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "hello\n" {
		t.Errorf("echo = %q", buf)
	}
}

// TestSwitchAll_SkipsDatabasesMissingTarget builds a Server with two
// databases — one declaring both local+staging, one declaring only local —
// and asserts that `use staging` switches the first while clearing the
// second (Skipped=true, no error).
func TestSwitchAll_SkipsDatabasesMissingTarget(t *testing.T) {
	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "app_dev"},
					{Name: "staging", Host: "127.0.0.1", Port: 5433, User: "u", Password: "p", Database: "app_staging"},
				},
			},
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "analytics",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "an_dev"},
				},
			},
		},
	}
	s, err := New(cfg, "local", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Both databases declare local — both should be active after New.
	for _, name := range []string{"appdb", "analytics"} {
		if _, ok := s.Databases()[name].Current(); !ok {
			t.Fatalf("%s should be active after init", name)
		}
	}

	results, err := s.SwitchAll("staging", nil)
	if err != nil {
		t.Fatalf("SwitchAll(staging): %v", err)
	}
	resByName := map[string]SwitchResult{}
	for _, r := range results {
		resByName[r.VirtualName] = r
	}
	if r := resByName["appdb"]; r.Skipped || r.Current != "staging" || r.CurrentDatabase != "app_staging" {
		t.Errorf("appdb result = %+v", r)
	}
	if r := resByName["analytics"]; !r.Skipped || r.Current != "" || r.Previous != "local" {
		t.Errorf("analytics result = %+v (want skipped from local)", r)
	}

	// Database state matches: appdb has staging, analytics is inactive.
	if cur, ok := s.Databases()["appdb"].Current(); !ok || cur.Name != "staging" {
		t.Errorf("appdb.Current after switch: ok=%v cur=%+v", ok, cur)
	}
	if _, ok := s.Databases()["analytics"].Current(); ok {
		t.Errorf("analytics should be inactive after switch to staging")
	}
}

// TestSwitchAll_AllSkippedErrors verifies that `use <bogus>` — where no
// database declares the target — fails cleanly without mutating state.
func TestSwitchAll_AllSkippedErrors(t *testing.T) {
	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "app_dev"},
				},
			},
		},
	}
	s, err := New(cfg, "local", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.SwitchAll("nonexistent", nil); err == nil {
		t.Fatal("expected error when no database declares the target")
	} else if !strings.Contains(err.Error(), "no databases declare") {
		t.Errorf("error = %v", err)
	}
	// State must not have changed.
	if cur, ok := s.Databases()["appdb"].Current(); !ok || cur.Name != "local" {
		t.Errorf("appdb should still be on local; got ok=%v cur=%+v", ok, cur)
	}
}

// TestSwitchAll_InactiveDatabaseGetsCleanPgError exercises the full
// handleStartup path: after `use staging` on a database that only declares
// local, a client connecting through the proxy must receive a PG-protocol
// ErrorResponse rather than blocking or panicking.
func TestSwitchAll_InactiveDatabaseGetsCleanPgError(t *testing.T) {
	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: freePort(t)},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "app_dev"},
					{Name: "staging", Host: "127.0.0.1", Port: 5433, User: "u", Password: "p", Database: "app_staging"},
				},
			},
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "analytics",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "an_dev"},
				},
			},
		},
	}
	s := startServer(t, cfg)
	defer s.Shutdown(context.Background())

	if _, err := s.SwitchAll("staging", nil); err != nil {
		t.Fatalf("SwitchAll: %v", err)
	}

	c, err := net.Dial("tcp", s.AddrFor(config.AdapterPostgres))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := SendStartupMessage(c, []KV{{K: "user", V: "u"}, {K: "database", V: "analytics"}}); err != nil {
		t.Fatal(err)
	}
	typ, body, err := ReadMessage(c)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if typ != 'E' {
		t.Errorf("typ = %c, want E", typ)
	}
	if !strings.Contains(string(body), "no active target") {
		t.Errorf("body = %q, want 'no active target'", body)
	}
}

// silence unused import warning
var _ = strconv.Itoa
