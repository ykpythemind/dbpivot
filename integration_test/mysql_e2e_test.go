//go:build integration_test

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/ykpythemind/dbpivot/internal/config"
	"github.com/ykpythemind/dbpivot/internal/control"
)

// TestScenario_MySQL_RouteAndRewriteDatabase brings up a real MySQL, creates
// two databases on it, and verifies that connecting to the proxy with
// dbname=<virtual-name> lands on the active target's configured database,
// before and after a `use` switch — the MySQL analog of the PostgreSQL
// route-and-rewrite scenario, validating the wire protocol against a real
// server (including the caching_sha2_password upstream-auth path).
func TestScenario_MySQL_RouteAndRewriteDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	my := startMySQL(t, ctx)
	my.createDatabase(t, ctx, "app_dev")
	my.createDatabase(t, ctx, "app_main_staging")

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterMySQL,
				VirtualName: "appdb",
				Targets: []config.Target{
					{
						Name: "local", Host: my.Host, Port: my.Port,
						User: my.User, Password: my.Pass, Database: "app_dev",
					},
					{
						Name: "staging", Host: my.Host, Port: my.Port,
						User: my.User, Password: my.Pass, Database: "app_${BRANCH}_staging",
					},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	if got := queryCurrentMySQLDatabase(t, ctx, d.Addr, "appdb"); got != "app_dev" {
		t.Errorf("default routing: DATABASE() = %q, want app_dev", got)
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

	if got := queryCurrentMySQLDatabase(t, ctx, d.Addr, "appdb"); got != "app_main_staging" {
		t.Errorf("post-switch: DATABASE() = %q, want app_main_staging", got)
	}
}

// TestScenario_MySQL_SwitchSwapsDataset puts the same schema into two
// databases with different rows, then verifies that a switch is visible as the
// data returned by a SELECT through the proxy — exercising the raw-piped
// command phase (result-set framing) end-to-end against a real MySQL.
func TestScenario_MySQL_SwitchSwapsDataset(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	my := startMySQL(t, ctx)
	my.createDatabase(t, ctx, "app_dev")
	my.createDatabase(t, ctx, "app_main_staging")

	const schema = `
CREATE TABLE items (
    id   INT PRIMARY KEY,
    name VARCHAR(255) NOT NULL
);`
	my.exec(t, ctx, "app_dev", schema)
	my.exec(t, ctx, "app_main_staging", schema)

	my.exec(t, ctx, "app_dev", `INSERT INTO items VALUES (1, 'dev-alpha'), (2, 'dev-beta')`)
	my.exec(t, ctx, "app_main_staging", `INSERT INTO items VALUES (10, 'staging-gamma'), (20, 'staging-delta'), (30, 'staging-epsilon')`)

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterMySQL,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: my.Host, Port: my.Port, User: my.User, Password: my.Pass, Database: "app_dev"},
					{Name: "staging", Host: my.Host, Port: my.Port, User: my.User, Password: my.Pass, Database: "app_${BRANCH}_staging"},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	if rows := selectMySQLItems(t, ctx, d.Addr, "appdb"); !equalRows(rows, []itemRow{
		{1, "dev-alpha"}, {2, "dev-beta"},
	}) {
		t.Errorf("default rows = %v, want dev dataset", rows)
	}

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

	if rows := selectMySQLItems(t, ctx, d.Addr, "appdb"); !equalRows(rows, []itemRow{
		{10, "staging-gamma"}, {20, "staging-delta"}, {30, "staging-epsilon"},
	}) {
		t.Errorf("post-switch rows = %v, want staging dataset", rows)
	}

	if _, err := control.Call(d.Sock, control.Request{Cmd: control.CmdUse, Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if rows := selectMySQLItems(t, ctx, d.Addr, "appdb"); !equalRows(rows, []itemRow{
		{1, "dev-alpha"}, {2, "dev-beta"},
	}) {
		t.Errorf("post-switch-back rows = %v, want dev dataset", rows)
	}
}

// TestScenario_MySQL_UnknownDatabaseErrorResponse verifies that connecting
// with a dbname that isn't a configured virtual database yields a clean
// MySQL-protocol ERR packet (surfaced as a normal driver connect error).
func TestScenario_MySQL_UnknownDatabaseErrorResponse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	my := startMySQL(t, ctx)
	my.createDatabase(t, ctx, "app_dev")

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterMySQL,
				VirtualName: "appdb",
				Targets: []config.Target{
					{Name: "local", Host: my.Host, Port: my.Port, User: my.User, Password: my.Pass, Database: "app_dev"},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	dsn := fmt.Sprintf("anyone:anything@tcp(%s)/nonexistent", d.Addr)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	if err := db.PingContext(ctx); err == nil {
		t.Fatal("expected connect error for unknown database")
	} else if !contains(err.Error(), "Unknown database") {
		t.Errorf("error %q does not mention 'Unknown database'", err.Error())
	}
}

func selectMySQLItems(t *testing.T, ctx context.Context, addr, dbname string) []itemRow {
	t.Helper()
	dsn := fmt.Sprintf("anyone:anything@tcp(%s)/%s", addr, dbname)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SELECT id, name FROM items ORDER BY id")
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
