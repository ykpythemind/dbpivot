//go:build integration_test

package integration_test

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"github.com/ykpythemind/dbpivot/internal/config"
)

// TestScenario_Mongo_ConnectAndCRUD brings up a real authenticated mongod and
// proves the headline objective end-to-end: the official MongoDB Go driver
// connects through dbpivot (no client credentials), and an insert/find round
// trip on a configured virtual database lands on the real upstream. It
// exercises the full live path — synthetic hello, the admin-command handshake
// chatter, deferred $db routing, upstream SCRAM-SHA-256 auth, the verbatim
// forward of the routing command (an insert carries a kind-1 document
// sequence), and the raw bidirectional pipe — against a real server, which the
// unit tests' fake mongod cannot guarantee.
func TestScenario_Mongo_ConnectAndCRUD(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	mg := startMongo(t, ctx)

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterMongo,
				VirtualName: "appdb",
				Targets: []config.Target{
					{
						Name: "local", Host: mg.Host, Port: mg.Port,
						User: mg.User, Password: mg.Pass, AuthSource: "admin",
					},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	cl := mongoClientThroughProxy(t, ctx, d.Addr)
	coll := cl.Database("appdb").Collection("items")

	want := []mongoItem{{ID: 1, Name: "alpha"}, {ID: 2, Name: "beta"}}
	if _, err := coll.InsertMany(ctx, []interface{}{want[0], want[1]}); err != nil {
		t.Fatalf("insert via proxy: %v", err)
	}

	if got := findMongoItems(t, ctx, coll); !equalItems(got, want) {
		t.Errorf("read-back via proxy = %v, want %v", got, want)
	}

	// Prove the writes actually reached the real upstream (not a local
	// responder) by reading them straight from the container, bypassing dbpivot.
	dc := mg.directClient(t, ctx)
	if got := findMongoItems(t, ctx, dc.Database("appdb").Collection("items")); !equalItems(got, want) {
		t.Errorf("read-back direct from upstream = %v, want %v", got, want)
	}
}

// TestScenario_Mongo_UnknownDatabaseError verifies that an operation naming a
// database that is not a configured virtual database yields a clean command
// error (surfaced by the driver), rather than hanging or being forwarded
// upstream. The connection itself still establishes — only the operation on the
// unconfigured database fails — because the hello/admin handshake is answered
// locally. A real command (insert) is used rather than ping, which the local
// admin responder would answer ok:1 regardless of $db.
func TestScenario_Mongo_UnknownDatabaseError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	mg := startMongo(t, ctx)

	cfg := &config.Config{
		Databases: []config.Database{
			{
				Adapter:     config.AdapterMongo,
				VirtualName: "appdb",
				Targets: []config.Target{
					{
						Name: "local", Host: mg.Host, Port: mg.Port,
						User: mg.User, Password: mg.Pass, AuthSource: "admin",
					},
				},
			},
		},
	}
	d := startDaemon(t, cfg, "local", nil)

	cl := mongoClientThroughProxy(t, ctx, d.Addr)
	err := cl.Database("nonexistent").Collection("x").
		FindOne(ctx, bson.D{}).Err()
	if err == nil {
		t.Fatal("expected error operating on an unconfigured database")
	}
	if !contains(err.Error(), "not configured") {
		t.Errorf("error %q does not mention 'not configured'", err.Error())
	}
}
