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
		Port: 6432,
		Databases: []config.Database{
			{
				VirtualName: "appdb",
				Default:     "local",
				Targets: []config.Target{
					{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "app_dev"},
					{Name: "staging", Host: "127.0.0.1", Port: 15432, User: "u", Password: "p", Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d, err := proxy.New(cfg, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if resp.Port != 6432 {
		t.Errorf("port = %d", resp.Port)
	}
	if len(resp.Databases) != 1 || resp.Databases[0].Current != "local" || resp.Databases[0].CurrentDatabase != "app_dev" {
		t.Errorf("databases = %+v", resp.Databases)
	}
}

func TestControl_Status_UnknownDatabase(t *testing.T) {
	sock, _, _ := setup(t)
	resp, _ := Call(sock, Request{Cmd: CmdStatus, VirtualName: "missing"})
	if resp.OK {
		t.Fatal("expected error")
	}
}

func TestControl_SwitchOK(t *testing.T) {
	sock, _, _ := setup(t)
	resp, err := Call(sock, Request{
		Cmd: CmdSwitch, VirtualName: "appdb", Target: "staging",
		Variables: map[string]string{"BRANCH": "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("not OK: %v", resp.Error)
	}
	if resp.Previous != "local" || resp.Current != "staging" || resp.CurrentDatabase != "app_main_staging" {
		t.Errorf("%+v", resp)
	}
}

func TestControl_SwitchMissingVariables(t *testing.T) {
	sock, _, _ := setup(t)
	resp, err := Call(sock, Request{Cmd: CmdSwitch, VirtualName: "appdb", Target: "staging"})
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

func TestControl_SwitchUnknownDatabase(t *testing.T) {
	sock, _, _ := setup(t)
	resp, _ := Call(sock, Request{Cmd: CmdSwitch, VirtualName: "nope", Target: "local"})
	if resp.OK {
		t.Fatal("expected error")
	}
}

func TestControl_List(t *testing.T) {
	sock, _, _ := setup(t)
	resp, err := Call(sock, Request{Cmd: CmdList})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.ListDatabases) != 1 {
		t.Fatalf("%+v", resp)
	}
	pl := resp.ListDatabases[0]
	if pl.VirtualName != "appdb" || pl.Default != "local" || pl.Current != "local" {
		t.Errorf("%+v", pl)
	}
	if len(pl.Targets) != 2 {
		t.Fatalf("targets = %v", pl.Targets)
	}
	if got := pl.Targets[1].RequiredVariables; len(got) != 1 || got[0] != "BRANCH" {
		t.Errorf("required = %v", got)
	}
}

func TestControl_UnknownCommand(t *testing.T) {
	sock, _, _ := setup(t)
	resp, _ := Call(sock, Request{Cmd: "frobnicate"})
	if resp.OK {
		t.Fatal("expected error")
	}
}
