//go:build integration_test

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	tcmongo "github.com/testcontainers/testcontainers-go/modules/mongodb"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// mongoContainer wraps a mongod testcontainer along with a directly usable
// host/port and the root credentials it was created with.
type mongoContainer struct {
	c    *tcmongo.MongoDBContainer
	Host string
	Port int
	User string
	Pass string
}

// startMongo boots a MongoDB container with a root user, which makes the image
// enable authentication. dbpivot always performs SCRAM on the upstream leg, so
// the upstream must require auth — connecting through the proxy then exercises
// the real saslStart/saslContinue path (mongoauth.go) against a live mongod.
func startMongo(t *testing.T, ctx context.Context) *mongoContainer {
	t.Helper()
	const (
		user = "root"
		pass = "integrationpass"
	)
	// mongo:7.0 advertises a newer wire version than the 6.0 the proxy
	// reports to clients; because dbpivot pipes raw after the handshake the
	// upstream still accepts the wire-17 commands, so this validates the
	// version-straddling path too.
	c, err := tcmongo.Run(ctx, "mongo:7.0",
		tcmongo.WithUsername(user),
		tcmongo.WithPassword(pass),
	)
	if err != nil {
		t.Fatalf("start mongo: %v", err)
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
	mp, err := c.MappedPort(ctx, "27017/tcp")
	if err != nil {
		t.Fatal(err)
	}

	mc := &mongoContainer{
		c:    c,
		Host: host,
		Port: int(mp.Num()),
		User: user,
		Pass: pass,
	}

	// The official mongo image starts a throwaway mongod to create the root
	// user, then restarts mongod with --auth. The module's wait strategy matches
	// the first "Waiting for connections" log (the throwaway), so the container
	// is reported ready while the auth-enabled mongod is still coming up —
	// connecting in that window yields a connection reset/EOF. Block until an
	// authenticated ping succeeds so the real upstream is genuinely reachable.
	waitMongoReady(t, ctx, mc)
	return mc
}

// waitMongoReady polls the mongod with the root credentials until an
// authenticated ping succeeds, ensuring the auth-enabled instance (not the
// throwaway bootstrap one) is accepting connections before the test proceeds.
func waitMongoReady(t *testing.T, ctx context.Context, mc *mongoContainer) {
	t.Helper()
	uri := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
		mc.User, mc.Pass, mc.Host, mc.Port)
	deadline := time.Now().Add(60 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		cl, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
		if err == nil {
			pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err = cl.Ping(pctx, nil)
			cancel()
			_ = cl.Disconnect(context.Background())
			if err == nil {
				return
			}
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("mongod did not become ready (auth-enabled): %v", lastErr)
}

// directClient connects straight to the upstream mongod (bypassing the proxy)
// with the root credentials, for seeding/verifying data out-of-band.
func (mc *mongoContainer) directClient(t *testing.T, ctx context.Context) *mongo.Client {
	t.Helper()
	uri := fmt.Sprintf("mongodb://%s:%s@%s:%d/?authSource=admin&directConnection=true",
		mc.User, mc.Pass, mc.Host, mc.Port)
	cl, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect upstream mongo: %v", err)
	}
	t.Cleanup(func() { _ = cl.Disconnect(context.Background()) })
	return cl
}

// mongoClientThroughProxy connects via dbpivot. No credentials are supplied:
// the proxy advertises auth as disabled on the client→proxy leg (it does SCRAM
// only upstream), so the driver proceeds straight to commands.
// directConnection=true keeps the driver from doing replica-set discovery and
// treats the proxy as the standalone it claims to be.
func mongoClientThroughProxy(t *testing.T, ctx context.Context, addr string) *mongo.Client {
	t.Helper()
	uri := fmt.Sprintf("mongodb://%s/?directConnection=true", addr)
	cl, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		t.Fatalf("connect proxy mongo: %v", err)
	}
	t.Cleanup(func() { _ = cl.Disconnect(context.Background()) })
	return cl
}

// mongoItem is a tiny document used to assert routed reads/writes line up.
type mongoItem struct {
	ID   int    `bson:"_id"`
	Name string `bson:"name"`
}

// findMongoItems returns every document in coll sorted by _id.
func findMongoItems(t *testing.T, ctx context.Context, coll *mongo.Collection) []mongoItem {
	t.Helper()
	cur, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		t.Fatalf("find: %v", err)
	}
	var out []mongoItem
	if err := cur.All(ctx, &out); err != nil {
		t.Fatalf("cursor.All: %v", err)
	}
	return out
}

func equalItems(a, b []mongoItem) bool {
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
