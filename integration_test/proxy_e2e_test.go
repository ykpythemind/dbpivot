//go:build integration_test

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ykpythemind/dbpivot/internal/config"
	"github.com/ykpythemind/dbpivot/internal/control"
)

// TestScenario_RouteAndRewriteDatabase brings up a real Postgres, creates
// two databases on it, and verifies that connecting to the proxy with
// dbname=<database-name> ends up on the target's configured database.
func TestScenario_RouteAndRewriteDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")
	pg.createDatabase(t, ctx, "app_main_staging")

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
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
	d := startDaemon(t, cfg, "local", nil)

	got := queryCurrentDatabase(t, ctx, d.Addr, "appdb")
	if got != "app_dev" {
		t.Errorf("default routing: current_database() = %q, want app_dev", got)
	}

	resp, err := control.Call(d.Sock, control.Request{
		Cmd: control.CmdUse, Target: "staging",
		Variables: map[string]string{"BRANCH": "main"},
	})
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if !resp.OK {
		t.Fatalf("switch not OK: %v (missing=%v)", resp.Error, resp.Missing)
	}
	if len(resp.Switched) != 1 || resp.Switched[0].CurrentDatabase != "app_main_staging" {
		t.Errorf("switched = %+v", resp.Switched)
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
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
					{Name: "staging", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

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
		Cmd: control.CmdUse, Target: "staging",
		Variables: map[string]string{"BRANCH": "main"},
	})
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if !resp.OK {
		t.Fatalf("switch not OK: %v", resp.Error)
	}
	if len(resp.Switched) != 1 || resp.Switched[0].ClosedConns < 1 {
		t.Errorf("expected closed_conns >= 1, got %+v", resp.Switched)
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

// TestScenario_UnknownDatabaseErrorResponse verifies that connecting with a
// dbname that isn't a configured database yields a clean PG-protocol
// ErrorResponse (visible as a normal pgx connect error).
func TestScenario_UnknownDatabaseErrorResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	dsn := fmt.Sprintf("postgres://anyone:any@%s/nonexistent?sslmode=disable", d.Addr)
	_, err := pgx.Connect(ctx, dsn)
	if err == nil {
		t.Fatal("expected connect error for unknown database")
	}
	if !contains(err.Error(), "not configured") {
		t.Errorf("error %q does not mention 'not configured'", err.Error())
	}
}

// TestScenario_Status smoke-checks the control plane `status` response.
func TestScenario_Status(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
					{Name: "staging", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	st, err := control.Call(d.Sock, control.Request{Cmd: control.CmdStatus})
	if err != nil {
		t.Fatal(err)
	}
	if !st.OK || st.CurrentTarget != "local" || len(st.Databases) != 1 || st.Databases[0].Current != "local" {
		t.Fatalf("status = %+v", st)
	}
}

// TestScenario_SwitchSwapsDataset puts the same schema into two databases
// with different rows, then verifies that a switch is visible as the data
// returned by a SELECT through the proxy.
func TestScenario_SwitchSwapsDataset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")
	pg.createDatabase(t, ctx, "app_main_staging")

	// Same schema, different rows.
	const schema = `
CREATE TABLE items (
    id   int PRIMARY KEY,
    name text NOT NULL
);`
	pg.exec(t, ctx, "app_dev", schema)
	pg.exec(t, ctx, "app_main_staging", schema)

	pg.exec(t, ctx, "app_dev", `INSERT INTO items VALUES (1, 'dev-alpha'), (2, 'dev-beta')`)
	pg.exec(t, ctx, "app_main_staging", `INSERT INTO items VALUES (10, 'staging-gamma'), (20, 'staging-delta'), (30, 'staging-epsilon')`)

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
					{Name: "staging", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	// 1. Default target → app_dev rows.
	if rows := selectItems(t, ctx, d.Addr, "appdb"); !equalRows(rows, []itemRow{
		{1, "dev-alpha"}, {2, "dev-beta"},
	}) {
		t.Errorf("default rows = %v, want dev dataset", rows)
	}

	// 2. Switch to staging.
	resp, err := control.Call(d.Sock, control.Request{
		Cmd: control.CmdUse, Target: "staging",
		Variables: map[string]string{"BRANCH": "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("switch: %v", resp.Error)
	}

	// 3. Same dbname=appdb on the client, but now we see the staging dataset.
	if rows := selectItems(t, ctx, d.Addr, "appdb"); !equalRows(rows, []itemRow{
		{10, "staging-gamma"}, {20, "staging-delta"}, {30, "staging-epsilon"},
	}) {
		t.Errorf("post-switch rows = %v, want staging dataset", rows)
	}

	// 4. Switch back, confirm we see the dev dataset again.
	if _, err := control.Call(d.Sock, control.Request{Cmd: control.CmdUse, Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if rows := selectItems(t, ctx, d.Addr, "appdb"); !equalRows(rows, []itemRow{
		{1, "dev-alpha"}, {2, "dev-beta"},
	}) {
		t.Errorf("post-switch-back rows = %v, want dev dataset", rows)
	}
}

type itemRow struct {
	ID   int
	Name string
}

func selectItems(t *testing.T, ctx context.Context, addr, dbname string) []itemRow {
	t.Helper()
	dsn := fmt.Sprintf("postgres://anyone:any@%s/%s?sslmode=disable", addr, dbname)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, "SELECT id, name FROM items ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	var out []itemRow
	for rows.Next() {
		var r itemRow
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}
	return out
}

func equalRows(a, b []itemRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestScenario_SwitchSkipsDatabaseMissingTarget exercises the relaxed
// validation: appdb declares both local+staging, analytics declares only
// local. After `use staging`, appdb is on staging and analytics is inactive
// — a client connecting through the proxy with dbname=analytics gets a
// clean PG ErrorResponse, while dbname=appdb succeeds.
func TestScenario_SwitchSkipsDatabaseMissingTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pg := startPostgres(t, ctx)
	pg.createDatabase(t, ctx, "app_dev")
	pg.createDatabase(t, ctx, "app_staging")
	pg.createDatabase(t, ctx, "an_dev")

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_dev"},
					{Name: "staging", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "app_staging"},
				},
			},
			{
				Adapter:     config.AdapterPostgres,
				VirtualName: "analytics",
				Targets: []config.Target{
					{Name: "local", Host: pg.Host, Port: pg.Port, User: pg.User, Password: pg.Pass, Database: "an_dev"},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	resp, err := control.Call(d.Sock, control.Request{Cmd: control.CmdUse, Target: "staging"})
	if err != nil {
		t.Fatalf("use staging: %v", err)
	}
	if !resp.OK {
		t.Fatalf("use staging not OK: %v", resp.Error)
	}
	var skipped bool
	for _, r := range resp.Switched {
		if r.VirtualName == "analytics" && r.Skipped {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("analytics should be marked Skipped in switch result: %+v", resp.Switched)
	}

	// appdb still works through the proxy on staging.
	if got := queryCurrentDatabase(t, ctx, d.Addr, "appdb"); got != "app_staging" {
		t.Errorf("appdb current_database = %q, want app_staging", got)
	}

	// analytics → clean error, not a hang or crash.
	dsn := fmt.Sprintf("postgres://anyone:any@%s/analytics?sslmode=disable", d.Addr)
	if _, err := pgx.Connect(ctx, dsn); err == nil {
		t.Fatal("expected error connecting to inactive analytics")
	} else if !contains(err.Error(), "no active target") {
		t.Errorf("error %q does not mention 'no active target'", err.Error())
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
