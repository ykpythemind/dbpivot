package proxy

import (
	"net"
	"strings"
	"testing"

	"github.com/ykpythemind/dbpivot/internal/config"
)

func buildPool(t *testing.T) *Pool {
	t.Helper()
	fwd := map[string]config.ForwardTarget{
		"ssm-staging": {Host: "127.0.0.1", Port: 15432},
	}
	p, err := NewPool(config.Pool{
		Name:    "appdb",
		Default: "local",
		Targets: []config.Target{
			{Name: "local", Host: "127.0.0.1", Port: 5432, User: "u", Password: "p", Database: "app_dev"},
			{Name: "staging", ForwardTo: "ssm-staging", User: "u", Password: "p", Database: "app_${BRANCH}_staging"},
			{Name: "prod", Host: "127.0.0.1", Port: 6543, User: "u", Password: "p", Database: "app_prod"},
		},
	}, fwd)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewPoolSetsDefault(t *testing.T) {
	p := buildPool(t)
	cur := p.Current()
	if cur.Name != "local" || cur.Database != "app_dev" || cur.Port != 5432 {
		t.Errorf("default current = %+v", cur)
	}
}

func TestSwitchUpdatesCurrent(t *testing.T) {
	p := buildPool(t)
	_, closed, _, err := p.Switch("staging", map[string]string{"BRANCH": "main"})
	if err != nil {
		t.Fatal(err)
	}
	if closed != 0 {
		t.Errorf("closed = %d, want 0", closed)
	}
	cur := p.Current()
	if cur.Name != "staging" || cur.Database != "app_main_staging" || cur.Host != "127.0.0.1" || cur.Port != 15432 {
		t.Errorf("current = %+v", cur)
	}
}

func TestSwitchUnknownTarget(t *testing.T) {
	p := buildPool(t)
	before := p.Current()
	_, _, _, err := p.Switch("qa", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("error: %v", err)
	}
	if p.Current() != before {
		t.Errorf("current changed unexpectedly")
	}
}

func TestSwitchMissingVariables(t *testing.T) {
	p := buildPool(t)
	before := p.Current()
	_, _, missing, err := p.Switch("staging", nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(missing) != 1 || missing[0] != "BRANCH" {
		t.Errorf("missing = %v", missing)
	}
	if p.Current() != before {
		t.Errorf("current changed unexpectedly")
	}
}

func TestSwitchInvalidResolvedDatabase(t *testing.T) {
	p := buildPool(t)
	_, _, _, err := p.Switch("staging", map[string]string{"BRANCH": "bad name"})
	if err == nil || !strings.Contains(err.Error(), "invalid characters") {
		t.Errorf("expected invalid-character error, got %v", err)
	}
}

func TestSwitchClosesExistingConns(t *testing.T) {
	p := buildPool(t)
	var conns []*Conn
	for i := 0; i < 3; i++ {
		a, b := net.Pipe()
		c, d := net.Pipe()
		conn := &Conn{Client: a, Upstream: c}
		p.Register(conn)
		conns = append(conns, conn)
		// keep refs so they don't get GCed
		_ = b
		_ = d
	}
	if got := p.ActiveConns(); got != 3 {
		t.Fatalf("active = %d", got)
	}

	_, closed, _, err := p.Switch("prod", nil)
	if err != nil {
		t.Fatal(err)
	}
	if closed != 3 {
		t.Errorf("closed = %d, want 3", closed)
	}
	if got := p.ActiveConns(); got != 0 {
		t.Errorf("active after switch = %d", got)
	}

	for _, c := range conns {
		if _, err := c.Client.Read(make([]byte, 1)); err == nil {
			t.Errorf("client read should have errored after close")
		}
	}
}

func TestRegistryNoLeak(t *testing.T) {
	p := buildPool(t)
	for i := 0; i < 100; i++ {
		a, _ := net.Pipe()
		c, _ := net.Pipe()
		conn := &Conn{Client: a, Upstream: c}
		p.Register(conn)
		conn.Close()
	}
	if got := p.ActiveConns(); got != 0 {
		t.Errorf("active after loop = %d", got)
	}
}
