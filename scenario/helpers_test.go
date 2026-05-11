//go:build scenario

package scenario

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ykpythemind/db-pool-switch/internal/config"
	"github.com/ykpythemind/db-pool-switch/internal/control"
	"github.com/ykpythemind/db-pool-switch/internal/proxy"
)

// pgContainer wraps a postgres testcontainer along with a directly usable
// host/port and the superuser credentials it was created with.
type pgContainer struct {
	c    *tcpg.PostgresContainer
	Host string
	Port int
	User string
	Pass string
}

func startPostgres(t *testing.T, ctx context.Context) *pgContainer {
	t.Helper()
	const (
		user  = "scenariouser"
		pass  = "scenariopass"
		bootDB = "postgres"
	)
	c, err := tcpg.Run(ctx,
		"postgres:16",
		tcpg.WithUsername(user),
		tcpg.WithPassword(pass),
		tcpg.WithDatabase(bootDB),
		testcontainers.WithEnv(map[string]string{
			// Force SCRAM-SHA-256 explicitly to match what we test.
			"POSTGRES_HOST_AUTH_METHOD":     "scram-sha-256",
			"POSTGRES_INITDB_ARGS":          "--auth-host=scram-sha-256 --auth-local=scram-sha-256",
			"POSTGRES_PASSWORD_ENCRYPTION":  "scram-sha-256",
		}),
		tcpg.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
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
	mp, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatal(err)
	}

	return &pgContainer{
		c:    c,
		Host: host,
		Port: int(mp.Num()),
		User: user,
		Pass: pass,
	}
}

// createDatabase issues a CREATE DATABASE against the container's bootstrap
// database. Names are passed through fmt.Sprintf — only call this with
// hard-coded identifiers from test code.
func (pc *pgContainer) createDatabase(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/postgres?sslmode=disable", pc.User, pc.Pass, pc.Host, pc.Port)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect for createDatabase: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %q", name)); err != nil {
		t.Fatalf("create db %q: %v", name, err)
	}
}

// freePort returns a TCP port that was free at the moment of the call.
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

func shortSocketPath(t *testing.T) string {
	t.Helper()
	name := fmt.Sprintf("dbps-%d.sock", rand.Int63())
	p := filepath.Join(os.TempDir(), name)
	t.Cleanup(func() { os.Remove(p) })
	return p
}

// scenarioDaemon bundles a running proxy.Server plus its control plane and
// records the addresses they listen on.
type scenarioDaemon struct {
	Server   *proxy.Server
	Control  *control.Server
	Sock     string
	Addr     string
	logger   *slog.Logger
}

// startDaemon writes cfg to a temp file, constructs Server + Control, starts
// them, and registers t.Cleanup to tear everything down.
func startDaemon(t *testing.T, cfg *config.Config) *scenarioDaemon {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	cfg.ControlSocket = shortSocketPath(t)
	if cfg.Port == 0 {
		cfg.Port = freePort(t)
	}

	srv, err := proxy.New(cfg, "", logger)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := control.Listen(cfg.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	cs := control.NewServer(ln, srv, cfg, logger)

	go srv.Start()
	go cs.Serve()

	// Wait for the TCP listener to be ready.
	deadline := time.Now().Add(3 * time.Second)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
	for time.Now().Before(deadline) {
		c, err := net.Dial("tcp", addr)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	d := &scenarioDaemon{
		Server:  srv,
		Control: cs,
		Sock:    cfg.ControlSocket,
		Addr:    addr,
		logger:  logger,
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = ln.Close()
		_ = os.Remove(cfg.ControlSocket)
	})
	return d
}

// queryCurrentDatabase opens a fresh pgx connection through the proxy and
// returns the upstream-reported current_database().
func queryCurrentDatabase(t *testing.T, ctx context.Context, addr, dbname string) string {
	t.Helper()
	// User/password aren't validated by the proxy (trust auth); whatever we
	// send is fine for the client→proxy hop.
	dsn := fmt.Sprintf("postgres://anyone:anything@%s/%s?sslmode=disable", addr, dbname)
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect proxy: %v", err)
	}
	defer conn.Close(ctx)

	var got string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&got); err != nil {
		t.Fatalf("query: %v", err)
	}
	return got
}
