package proxy

import (
	"net"
	"strings"
	"testing"

	"github.com/ykpythemind/dbpivot/internal/config"
)

func buildDatabase(t *testing.T) *Database {
	t.Helper()
	fwd := map[string]config.ForwardTarget{
		"ssm-staging": {Host: "127.0.0.1", Port: 15432},
	}
	return NewDatabase(config.Database{
		VirtualName: "appdb",
		Targets: []config.Target{
			{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "app_dev"},
			{Name: "staging", ForwardTo: "ssm-staging", User: "u", Password: "p", Database: "app_${BRANCH}_staging"},
			{Name: "prod", Host: "127.0.0.1", Port: 6543, User: "u", Password: "p", Database: "app_prod"},
		},
	}, fwd)
}

// activate is a convenience wrapper used by tests: resolve + apply.
func activate(t *testing.T, d *Database, target string, vars map[string]string) ResolvedTarget {
	t.Helper()
	rt, _, err := d.ResolveTarget(target, vars)
	if err != nil {
		t.Fatalf("resolve %q: %v", target, err)
	}
	d.Apply(rt)
	return rt
}

func TestNewDatabaseIsInactiveUntilApply(t *testing.T) {
	d := buildDatabase(t)
	if _, ok := d.Current(); ok {
		t.Errorf("fresh database should report no current target")
	}
	activate(t, d, "local", nil)
	cur, ok := d.Current()
	if !ok || cur.Name != "local" || cur.Database != "app_dev" || cur.Port != 5432 {
		t.Errorf("after activate: ok=%v cur=%+v", ok, cur)
	}
}

func TestApplyUpdatesCurrent(t *testing.T) {
	d := buildDatabase(t)
	activate(t, d, "local", nil)
	activate(t, d, "staging", map[string]string{"BRANCH": "main"})
	cur, ok := d.Current()
	if !ok || cur.Name != "staging" || cur.Database != "app_main_staging" || cur.Host != "127.0.0.1" || cur.Port != 15432 {
		t.Errorf("current = %+v", cur)
	}
}

func TestResolveUnknownTarget(t *testing.T) {
	d := buildDatabase(t)
	_, _, err := d.ResolveTarget("qa", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("error: %v", err)
	}
}

func TestResolveMissingVariables(t *testing.T) {
	d := buildDatabase(t)
	_, missing, err := d.ResolveTarget("staging", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(missing) != 1 || missing[0] != "BRANCH" {
		t.Errorf("missing = %v", missing)
	}
}

func TestResolveInvalidResolvedDatabase(t *testing.T) {
	d := buildDatabase(t)
	_, _, err := d.ResolveTarget("staging", map[string]string{"BRANCH": "bad name"})
	if err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("expected invalid-character error, got %v", err)
	}
}

func TestApplyClosesExistingConns(t *testing.T) {
	d := buildDatabase(t)
	activate(t, d, "local", nil)
	var conns []*Conn
	for i := 0; i < 3; i++ {
		a, b := net.Pipe()
		c, dEnd := net.Pipe()
		conn := &Conn{Client: a, Upstream: c}
		d.Register(conn)
		conns = append(conns, conn)
		_ = b
		_ = dEnd
	}
	if got := d.ActiveConns(); got != 3 {
		t.Fatalf("active = %d", got)
	}

	rt, _, err := d.ResolveTarget("prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	prev, closed := d.Apply(rt)
	if prev == nil || prev.Name != "local" {
		t.Errorf("prev = %+v", prev)
	}
	if closed != 3 {
		t.Errorf("closed = %d, want 3", closed)
	}
	if got := d.ActiveConns(); got != 0 {
		t.Errorf("active after switch = %d", got)
	}
	for _, c := range conns {
		if _, err := c.Client.Read(make([]byte, 1)); err == nil {
			t.Errorf("client read should have errored after close")
		}
	}
}

func TestRegistryNoLeak(t *testing.T) {
	d := buildDatabase(t)
	activate(t, d, "local", nil)
	for i := 0; i < 100; i++ {
		a, _ := net.Pipe()
		c, _ := net.Pipe()
		conn := &Conn{Client: a, Upstream: c}
		d.Register(conn)
		conn.Close()
	}
	if got := d.ActiveConns(); got != 0 {
		t.Errorf("active after loop = %d", got)
	}
}
