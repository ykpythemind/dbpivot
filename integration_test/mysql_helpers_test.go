//go:build integration_test

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
)

// myContainer wraps a mysql testcontainer along with a directly usable
// host/port and the root credentials it was created with.
type myContainer struct {
	c    *tcmysql.MySQLContainer
	Host string
	Port int
	User string
	Pass string
}

// startMySQL boots a MySQL 8 container as root so test code can create
// databases freely. MySQL 8 defaults to caching_sha2_password, so logging in
// over the plaintext upstream link exercises the proxy's caching_sha2
// full-auth RSA path against a real server.
func startMySQL(t *testing.T, ctx context.Context) *myContainer {
	t.Helper()
	const (
		user   = "root"
		pass   = "integrationpass"
		bootDB = "bootstrap"
	)
	c, err := tcmysql.Run(ctx,
		"mysql:8.0.36",
		tcmysql.WithUsername(user),
		tcmysql.WithPassword(pass),
		tcmysql.WithDatabase(bootDB),
	)
	if err != nil {
		t.Fatalf("start mysql: %v", err)
	}
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(shutCtx)
	})

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := c.MappedPort(ctx, "3306/tcp")
	if err != nil {
		t.Fatal(err)
	}

	return &myContainer{
		c:    c,
		Host: host,
		Port: int(mp.Num()),
		User: user,
		Pass: pass,
	}
}

// dsnDirect builds a go-sql-driver DSN straight to the upstream MySQL
// (bypassing the proxy) for the given database.
func (mc *myContainer) dsnDirect(database string) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%d)/%s", mc.User, mc.Pass, mc.Host, mc.Port, database)
}

// createDatabase issues CREATE DATABASE against the container. Names are
// interpolated — only call with hard-coded identifiers from test code.
func (mc *myContainer) createDatabase(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	db, err := sql.Open("mysql", mc.dsnDirect(""))
	if err != nil {
		t.Fatalf("open for createDatabase: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE `%s`", name)); err != nil {
		t.Fatalf("create db %q: %v", name, err)
	}
}

// exec opens a fresh connection straight to the upstream MySQL (bypassing the
// proxy) for the given database and runs sql.
func (mc *myContainer) exec(t *testing.T, ctx context.Context, database, query string) {
	t.Helper()
	db, err := sql.Open("mysql", mc.dsnDirect(database))
	if err != nil {
		t.Fatalf("open %s: %v", database, err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, query); err != nil {
		t.Fatalf("exec on %s: %v\nSQL:\n%s", database, err, query)
	}
}

// queryCurrentMySQLDatabase opens a fresh connection through the proxy and
// returns the upstream-reported DATABASE(). User/password are not validated by
// the proxy (trust auth), so any credentials suffice for the client→proxy hop.
func queryCurrentMySQLDatabase(t *testing.T, ctx context.Context, addr, dbname string) string {
	t.Helper()
	dsn := fmt.Sprintf("anyone:anything@tcp(%s)/%s", addr, dbname)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open proxy: %v", err)
	}
	defer db.Close()

	var got string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	return got
}
