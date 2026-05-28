package control

import (
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/ykpythemind/dbpivot/internal/config"
	"github.com/ykpythemind/dbpivot/internal/proxy"
)

// shortSocketPath returns a path under /tmp short enough to satisfy macOS's
// 104-byte limit even when the test name is long.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("dbps-%d.sock", rand.Int63())
	p := filepath.Join(os.TempDir(), name)
	t.Cleanup(func() { os.Remove(p) })
	return p
}

func setup(t *testing.T) (sockPath string, srv *Server, dm *proxy.Server) {
	t.Helper()
	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: 6432},
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "app_dev"},
					{Name: "staging", Host: "127.0.0.1", Port: 15432, User: "u", Password: "p", Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d, err := proxy.New(cfg, "local", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	sockPath = shortSocketPath(t)
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv = NewServer(ln, d, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go srv.Serve()
	return sockPath, srv, d
}

func TestControl_Status(t *testing.T) {
	sock, _, _ := setup(t)
	resp, err := Call(sock, Request{Cmd: CmdStatus})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("not OK: %v", resp.Error)
	}
	if resp.Ports[config.AdapterPostgres] != 6432 {
		t.Errorf("ports = %v", resp.Ports)
	}
	if resp.CurrentTarget != "local" {
		t.Errorf("current_target = %q", resp.CurrentTarget)
	}
	if len(resp.Databases) != 1 || resp.Databases[0].Current != "local" || resp.Databases[0].CurrentDatabase != "app_dev" {
		t.Errorf("databases = %+v", resp.Databases)
	}
}

func TestControl_UseOK(t *testing.T) {
	sock, _, _ := setup(t)
	resp, err := Call(sock, Request{
		Cmd: CmdUse, Target: "staging",
		Variables: map[string]string{"BRANCH": "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("not OK: %v", resp.Error)
	}
	if resp.Target != "staging" {
		t.Errorf("target = %q", resp.Target)
	}
	if len(resp.Switched) != 1 {
		t.Fatalf("switched = %+v", resp.Switched)
	}
	r := resp.Switched[0]
	if r.VirtualName != "appdb" || r.Previous != "local" || r.Current != "staging" || r.CurrentDatabase != "app_main_staging" {
		t.Errorf("%+v", r)
	}
}

func TestControl_UseMissingVariables(t *testing.T) {
	sock, _, _ := setup(t)
	resp, err := Call(sock, Request{Cmd: CmdUse, Target: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Fatal("expected error")
	}
	if len(resp.Missing) != 1 || resp.Missing[0] != "BRANCH" {
		t.Errorf("missing = %v", resp.Missing)
	}
}

func TestControl_UseUnknownTarget(t *testing.T) {
	sock, _, _ := setup(t)
	resp, _ := Call(sock, Request{Cmd: CmdUse, Target: "nope"})
	if resp.OK {
		t.Fatal("expected error")
	}
}

// TestControl_UsePartialSkipsAndInactiveStatus verifies that a `use` against
// a target only some databases declare succeeds, the skipped database
// appears in Switched with Skipped=true, and a subsequent `status` reports
// it as Inactive.
func TestControl_UsePartialSkipsAndInactiveStatus(t *testing.T) {
	cfg := &config.Config{
		ListenPorts: map[string]int{config.AdapterPostgres: 6432},
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
	d, err := proxy.New(cfg, "local", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	sockPath := shortSocketPath(t)
	ln, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(ln, d, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	go srv.Serve()

	resp, err := Call(sockPath, Request{Cmd: CmdUse, Target: "staging"})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("not OK: %v", resp.Error)
	}
	byName := map[string]SwitchResult{}
	for _, r := range resp.Switched {
		byName[r.VirtualName] = r
	}
	if r := byName["appdb"]; r.Skipped || r.Current != "staging" {
		t.Errorf("appdb: %+v (want switched to staging)", r)
	}
	if r := byName["analytics"]; !r.Skipped || r.Previous != "local" {
		t.Errorf("analytics: %+v (want skipped from local)", r)
	}

	st, err := Call(sockPath, Request{Cmd: CmdStatus})
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.CurrentTarget != "staging" {
		t.Fatalf("status: %+v", st)
	}
	stByName := map[string]DatabaseStatus{}
	for _, s := range st.Databases {
		stByName[s.VirtualName] = s
	}
	if s := stByName["appdb"]; s.Inactive || s.Current != "staging" {
		t.Errorf("appdb status: %+v", s)
	}
	if s := stByName["analytics"]; !s.Inactive {
		t.Errorf("analytics status: %+v (want Inactive)", s)
	}
}

func TestControl_UnknownCommand(t *testing.T) {
	sock, _, _ := setup(t)
	resp, _ := Call(sock, Request{Cmd: "frobnicate"})
	if resp.OK {
		t.Fatal("expected error")
	}
}
