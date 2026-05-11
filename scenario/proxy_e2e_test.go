//go:build scenario

package scenario

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ykpythemind/db-pool-switch/internal/config"
	"github.com/ykpythemind/db-pool-switch/internal/control"
)

// TestScenario_RouteAndRewriteDatabase brings up a real Postgres, creates
// two databases on it, and verifies that connecting to the proxy with
// dbname=<pool-name> ends up on the target's configured database.
func TestScenario_RouteAndRewriteDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")
	pg.createDatabase(t, ctx, "app_main_staging")

	cfg := &config.Config{
		Pools: []config.Pool{
			{
				Name:    "appdb",
				Default: "local",
				Targets: []config.Target{
					{
						Name: "local", Host: pg.Host, Port: pg.Port,
						User: pg.User, Password: pg.Pass, Database: "app_dev",
					},
					{
						Name: "staging", Host: pg.Host, Port: pg.Port,
						User: pg.User, Password: pg.Pass, Database: "app_${BRANCH}_staging",
					},
				},
			},
		},
	}
	d := startDaemon(t, cfg)

	got := queryCurrentDatabase(t, ctx, d.Addr, "appdb")
	if got != "app_dev" {
		t.Errorf("default routing: current_database() = %q, want app_dev", got)
	}

	resp, err := control.Call(d.Sock, control.Request{
		Cmd: control.CmdSwitch, Pool: "appdb", Target: "staging",
		Variables: map[string]string{"BRANCH": "main"},
	})
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if !resp.OK {
		t.Fatalf("switch not OK: %v (missing=%v)", resp.Error, resp.Missing)
	}
	if resp.CurrentDatabase != "app_main_staging" {
		t.Errorf("switch current_database = %q, want app_main_staging", resp.CurrentDatabase)
	}

	got = queryCurrentDatabase(t, ctx, d.Addr, "appdb")
	if got != "app_main_staging" {
		t.Errorf("post-switch: current_database() = %q, want app_main_staging", got)
	}
}

// TestScenario_SwitchDropsExistingConn opens a connection, performs a switch
// while it is held, and asserts that the next operation on the old
// connection fails (proxy-initiated close) while a fresh connect lands on
// the new database.
func TestScenario_SwitchDropsExistingConn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")
	pg.createDatabase(t, ctx, "app_main_staging")

	cfg := &config.Config{
		Pools: []config.Pool{
			{
				Name:    "appdb",
				Default: "local",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
					{Name: "staging", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d := startDaemon(t, cfg)

	dsn := fmt.Sprintf("postgres://anyone:any@%s/appdb?sslmode=disable", d.Addr)
	long, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("open long-lived conn: %v", err)
	}
	defer long.Close(context.Background())

	// Sanity: it points at app_dev right now.
	var db string
	if err := long.QueryRow(ctx, "SELECT current_database()").Scan(&db); err != nil {
		t.Fatalf("pre-switch query: %v", err)
	}
	if db != "app_dev" {
		t.Fatalf("pre-switch db = %q", db)
	}

	resp, err := control.Call(d.Sock, control.Request{
		Cmd: control.CmdSwitch, Pool: "appdb", Target: "staging",
		Variables: map[string]string{"BRANCH": "main"},
	})
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if !resp.OK {
		t.Fatalf("switch not OK: %v", resp.Error)
	}
	if resp.ClosedConns < 1 {
		t.Errorf("expected closed_conns >= 1, got %d", resp.ClosedConns)
	}

	// The old connection should be dead now.
	short, qcancel := context.WithTimeout(ctx, 2*time.Second)
	defer qcancel()
	if err := long.QueryRow(short, "SELECT 1").Scan(new(int)); err == nil {
		t.Errorf("expected old connection to error after switch")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("(old conn error as expected: %v)", err)
	}

	got := queryCurrentDatabase(t, ctx, d.Addr, "appdb")
	if got != "app_main_staging" {
		t.Errorf("post-switch fresh conn: %q, want app_main_staging", got)
	}
}

// TestScenario_UnknownPoolErrorResponse verifies that connecting with a
// dbname that isn't a configured pool yields a clean PG-protocol
// ErrorResponse (visible as a normal pgx connect error).
func TestScenario_UnknownPoolErrorResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")

	cfg := &config.Config{
		Pools: []config.Pool{
			{
				Name:    "appdb",
				Default: "local",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
				},
			},
		},
	}
	d := startDaemon(t, cfg)

	dsn := fmt.Sprintf("postgres://anyone:any@%s/nonexistent?sslmode=disable", d.Addr)
	_, err := pgx.Connect(ctx, dsn)
	if err == nil {
		t.Fatal("expected connect error for unknown pool")
	}
	if !contains(err.Error(), "not configured") {
		t.Errorf("error %q does not mention 'not configured'", err.Error())
	}
}

// TestScenario_StatusAndList smoke-checks the control plane responses for
// `status` and `list`.
func TestScenario_StatusAndList(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")

	cfg := &config.Config{
		Pools: []config.Pool{
			{
				Name:    "appdb",
				Default: "local",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
					{Name: "staging", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d := startDaemon(t, cfg)

	st, err := control.Call(d.Sock, control.Request{Cmd: control.CmdStatus})
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || len(st.Pools) != 1 || st.Pools[0].Current != "local" {
		t.Fatalf("status = %+v", st)
	}

	lst, err := control.Call(d.Sock, control.Request{Cmd: control.CmdList})
	if err != nil {
		t.Fatal(err)
	}
	if !lst.OK || len(lst.ListPools) != 1 {
		t.Fatalf("list = %+v", lst)
	}
	if got := lst.ListPools[0].Targets[1].RequiredVariables; len(got) != 1 || got[0] != "BRANCH" {
		t.Errorf("required_variables = %v", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
