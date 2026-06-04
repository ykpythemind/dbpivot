package proxy

import (
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/ykpythemind/dbpivot/internal/config"
)

// MongoDB connection handler (client-first leg).
//
// This is the MongoDB analog of server.go's dispatch/handleStartup (PG) and
// mysqlserver.go's dispatchMySQL: it ties together the pure building blocks
// from mongowire.go / mongohandshake.go / mongoauth.go into the live server.
//
// Two MongoDB-specific shapes drive the control flow here:
//
//   - Auth is advertised as disabled to the client. The client→proxy leg cannot
//     offer SCRAM "trust" auth, so BuildHelloReply omits saslSupportedMechs and
//     the client proceeds straight to commands; all real authentication happens
//     on the upstream leg via AuthenticateUpstreamMongo.
//
//   - Routing is deferred. MongoDB carries the target database in $db on every
//     command, and the initial hello is always against `admin`, so the proxy
//     answers hello locally and waits for the first command whose $db names a
//     configured virtual database before it can pick (and dial) an upstream.
//     Once routed, the command that triggered routing is forwarded verbatim and
//     the connection is piped raw, exactly like the PG/MySQL command phase.

// MongoDB server error codes used when the proxy itself rejects a command,
// mirroring mysqlserver.go's er* constants. These are surfaced as ok:0 replies.
const (
	mongoErrInternalError    = 1  // no active target, internal failure
	mongoErrHostUnreachable  = 6  // upstream dial/auth failed
	mongoErrInvalidNamespace = 73 // $db names no configured database
)

// dispatchMongo serves one client connection on the MongoDB leg. It answers the
// handshake (and any pre-routing hello probes) locally, then routes on the first
// command that names a configured database.
func (s *Server) dispatchMongo(client net.Conn) error {
	connID := int32(s.connSeq.Add(1))

	for {
		hdr, body, err := ReadMongoMessage(client)
		if err != nil {
			return fmt.Errorf("mongo read command: %w", err)
		}
		cmd, err := decodeMongoCommand(hdr, body)
		if err != nil {
			return err
		}

		// Routable? The first command whose $db names a configured Mongo
		// database binds the connection to that upstream for its lifetime.
		if database := s.lookupDatabase(config.AdapterMongo, cmd.DB); database != nil {
			return s.routeMongo(client, database, cmd.DB, hdr, body)
		}

		// Not routable yet. Answer hello/isMaster ourselves (it always targets
		// `admin`); reject anything else with a command error. Serving the
		// broader set of pre-routing admin commands (ping, buildInfo, ...) would
		// need a local responder and is left to a later iteration.
		if IsHelloCommand(cmd.Doc) {
			if err := WriteMongoReply(client, hdr.OpCode, hdr.RequestID, BuildHelloReply(connID, time.Now())); err != nil {
				return fmt.Errorf("mongo write hello reply: %w", err)
			}
			continue
		}
		reply := mongoErrorReply(fmt.Sprintf("dbpivot: database %q is not configured for mongodb", cmd.DB), mongoErrInvalidNamespace)
		if err := WriteMongoReply(client, hdr.OpCode, hdr.RequestID, reply); err != nil {
			return fmt.Errorf("mongo write error reply: %w", err)
		}
		s.logger.Info("unknown database", "dbname", cmd.DB, "command", cmd.CommandName(), "remote", client.RemoteAddr())
	}
}

// routeMongo dials and authenticates the upstream for database, forwards the
// command that triggered routing (hdr/body, verbatim), then pipes the
// connection raw. db is the client-named $db (for diagnostics); the upstream
// SCRAM auth runs with the resolved target's configured credentials.
func (s *Server) routeMongo(client net.Conn, database *Database, db string, hdr MsgHeader, body []byte) error {
	rt, ok := database.Current()
	if !ok {
		_ = WriteMongoReply(client, hdr.OpCode, hdr.RequestID,
			mongoErrorReply(fmt.Sprintf("database %q has no active target", db), mongoErrInternalError))
		return nil
	}

	addr := net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port))
	up, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		_ = WriteMongoReply(client, hdr.OpCode, hdr.RequestID,
			mongoErrorReply(fmt.Sprintf("upstream dial failed: %v", err), mongoErrHostUnreachable))
		s.logger.Error("upstream dial", "err", err, "addr", addr)
		return err
	}

	// All authentication happens on this leg (the client was told auth is
	// disabled). The credentials live in the upstream's auth database; dbpivot
	// has no authSource config field yet, so AuthenticateUpstreamMongo defaults
	// it to "admin" — the conventional home for MongoDB user credentials.
	if err := AuthenticateUpstreamMongo(up, rt.User, rt.Password, ""); err != nil {
		_ = WriteMongoReply(client, hdr.OpCode, hdr.RequestID,
			mongoErrorReply(fmt.Sprintf("upstream auth failed: %v", err), mongoErrHostUnreachable))
		up.Close()
		s.logger.Error("upstream auth", "virtual_name", database.VirtualName(), "err", err)
		return err
	}

	// Forward the routing command verbatim, preserving the client's RequestID so
	// the upstream reply's ResponseTo correlates when piped straight back. Using
	// the raw body keeps any kind-1 document sequences / checksum intact.
	if err := WriteMongoMessage(up, hdr.RequestID, hdr.ResponseTo, hdr.OpCode, body); err != nil {
		up.Close()
		return fmt.Errorf("mongo forward first command: %w", err)
	}

	conn := &Conn{Client: client, Upstream: up, Resolved: rt}
	database.Register(conn)
	defer conn.Close()

	pipeBidi(client, up)
	return nil
}

// mongoErrorReply builds an ok:0 command reply carrying errmsg and (when
// non-zero) code, the shape MongoDB drivers expect for a failed command.
func mongoErrorReply(errmsg string, code int32) BSON {
	doc := BSON{{Key: "ok", Value: 0.0}}
	if errmsg != "" {
		doc = append(doc, BSONElem{Key: "errmsg", Value: errmsg})
	}
	if code != 0 {
		doc = append(doc, BSONElem{Key: "code", Value: code})
	}
	return doc
}
