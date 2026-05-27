package proxy

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"

	"github.com/ykpythemind/dbpivot/internal/config"
)

// MySQL connection handler (server-first leg).
//
// This is the MySQL analog of server.go's dispatch + handleStartup. Unlike
// PostgreSQL (client-first: the client opens with a StartupMessage carrying the
// dbname), MySQL is server-first: the proxy greets the client with its own
// Handshake v10, then reads the client's HandshakeResponse41 to learn the
// target dbname for routing. Only after the upstream login succeeds does the
// proxy send the concluding OK packet, mirroring how the PG path defers
// AuthenticationOk until AuthenticateUpstream succeeds.

// mysqlServerVersion is the server_version string the proxy advertises in its
// greeting. The "8.0" prefix keeps clients that gate features on the reported
// version on the modern (caching_sha2 / DEPRECATE_EOF capable) code path.
const mysqlServerVersion = "8.0.0-dbpivot"

// MySQL server error codes used when the proxy itself rejects a connection.
const (
	erAccessDenied = 1045 // SQLSTATE 28000
	erBadDBError   = 1049 // SQLSTATE 42000
	erUnknownError = 1105 // SQLSTATE HY000
)

// writeMySQLErr sends an ERR packet to conn with the given sequence id.
func writeMySQLErr(w io.Writer, seq byte, code uint16, sqlState, msg string) error {
	_, err := WritePacket(w, seq, EncodeERRPacket(code, sqlState, msg))
	return err
}

// dispatchMySQL serves one client connection on the MySQL leg: greet, route by
// dbname, dial + authenticate the upstream, conclude with OK, then pipe the
// command phase through unchanged.
func (s *Server) dispatchMySQL(client net.Conn) error {
	connID := s.connSeq.Add(1)
	login, err := GreetClientMySQL(client, mysqlServerVersion, connID)
	if err != nil {
		return fmt.Errorf("mysql client handshake: %w", err)
	}

	// Routing key: the client-supplied database, falling back to the username
	// (mirrors the PG path, where dbname defaults to user when unset).
	dbname := login.Response.Database
	if dbname == "" {
		dbname = login.Response.Username
	}

	database := s.lookupDatabase(dbname)
	if database == nil {
		_ = writeMySQLErr(client, login.NextSeq, erBadDBError, "42000", fmt.Sprintf("Unknown database '%s'", dbname))
		s.logger.Info("unknown database", "dbname", dbname, "remote", client.RemoteAddr())
		return nil
	}

	rt, ok := database.Current()
	if !ok {
		_ = writeMySQLErr(client, login.NextSeq, erUnknownError, "HY000", fmt.Sprintf("database %q has no active target", dbname))
		return nil
	}

	up, err := net.DialTimeout("tcp", net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)), dialTimeout)
	if err != nil {
		_ = writeMySQLErr(client, login.NextSeq, erUnknownError, "HY000", fmt.Sprintf("upstream dial failed: %v", err))
		s.logger.Error("upstream dial", "err", err, "addr", net.JoinHostPort(rt.Host, strconv.Itoa(rt.Port)))
		return err
	}

	// Upstream database: the resolved target database, or pass through the
	// client-supplied dbname when the target leaves it empty.
	upDatabase := rt.Database
	if upDatabase == "" {
		upDatabase = login.Response.Database
	}

	// sslmode=require negotiates in-band TLS to the upstream (CLIENT_SSL),
	// encrypting the link without verifying the server certificate — matching
	// the PostgreSQL path's sslmode=require semantics. Otherwise the leg is
	// plaintext.
	var tlsConfig *tls.Config
	if rt.SSLMode == config.SSLModeRequire {
		tlsConfig = &tls.Config{
			ServerName:         rt.Host,
			InsecureSkipVerify: true, // sslmode=require: encrypt, don't verify
		}
	}

	upConn, _, upCaps, err := AuthenticateUpstreamMySQL(up, rt.User, rt.Password, upDatabase, false, tlsConfig)
	if err != nil {
		_ = writeMySQLErr(client, login.NextSeq, erAccessDenied, "28000", fmt.Sprintf("upstream auth failed: %v", err))
		up.Close()
		s.logger.Error("upstream auth", "virtual_name", database.VirtualName(), "err", err)
		return err
	}
	// After a successful login, pipe over the (possibly TLS-wrapped) conn.
	up = upConn

	// Capability symmetry: after login the command phase is piped RAW, so the
	// result-set framing the client expects must match what the upstream emits.
	// CLIENT_DEPRECATE_EOF is the bit that changes that framing; if the two
	// legs disagree on it, fail loudly rather than silently corrupt traffic.
	if login.Caps&ClientDeprecateEOF != upCaps&ClientDeprecateEOF {
		_ = writeMySQLErr(client, login.NextSeq, erUnknownError, "HY000", "proxy/upstream capability mismatch (CLIENT_DEPRECATE_EOF)")
		up.Close()
		s.logger.Error("mysql capability mismatch", "virtual_name", database.VirtualName(),
			"client_caps", login.Caps, "upstream_caps", upCaps)
		return fmt.Errorf("mysql capability mismatch: client=0x%08x upstream=0x%08x", login.Caps, upCaps)
	}

	// Upstream login succeeded — conclude the client handshake with OK.
	if _, err := WritePacket(client, login.NextSeq, EncodeOKPacket(0, 0, serverStatusAutocommit, 0)); err != nil {
		up.Close()
		return fmt.Errorf("write OK to client: %w", err)
	}

	conn := &Conn{Client: client, Upstream: up, Resolved: rt}
	database.Register(conn)
	defer conn.Close()

	pipeBidi(client, up)
	return nil
}
