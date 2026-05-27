package proxy

import (
	"crypto/rand"
	"fmt"
	"io"
)

// MySQL client-facing handshake (server side of the client→proxy leg).
//
// This is the MySQL analog of the PostgreSQL StartupMessage handling in
// server.go's handleStartup: the proxy greets a connecting client with its own
// Handshake v10, reads the client's HandshakeResponse41, and — because the
// client→proxy leg is trust auth (any password accepted) — does NOT verify the
// password. It deliberately does not send the final OK packet: like the PG path
// (which only writes AuthenticationOk after AuthenticateUpstream succeeds), the
// caller sends OK only once the upstream login succeeds, so an upstream failure
// can be surfaced to the client as an ERR packet instead.

// serverStatusAutocommit is the initial SERVER_STATUS flag the proxy reports in
// its greeting (matches a freshly-connected, autocommit session).
const serverStatusAutocommit = 0x0002

// mysqlProxyClientCaps is the fixed capability set the proxy advertises when it
// greets a connecting MySQL client. It is deliberately conservative and
// CLIENT_SSL is intentionally omitted (the client→proxy leg is plaintext trust,
// mirroring how the PostgreSQL path rejects SSLRequest). Because dbpivot pipes
// the command phase RAW after login, this set must stay reconcilable with
// mysqlUpstreamCaps so both legs use identical packet formats (CLIENT_DEPRECATE_EOF etc.).
const mysqlProxyClientCaps = ClientLongPassword |
	ClientLongFlag |
	ClientProtocol41 |
	ClientTransactions |
	ClientSecureConnection |
	ClientPluginAuth |
	ClientPluginAuthLenencClientData |
	ClientConnectWithDB |
	ClientDeprecateEOF

// newMySQLSalt returns a fresh 20-byte auth-plugin-data nonce. NUL bytes are
// replaced because the protocol NUL-terminates the auth-plugin-data part-2 and
// scramble inputs must round-trip through that framing unchanged.
func newMySQLSalt() ([]byte, error) {
	salt := make([]byte, 20)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("generate salt: %w", err)
	}
	for i, b := range salt {
		if b == 0 {
			salt[i] = 1
		}
	}
	return salt, nil
}

// ClientLogin captures the outcome of the client-facing MySQL handshake.
type ClientLogin struct {
	Response *HandshakeResponse41
	// Caps is the capability set negotiated on the client↔proxy leg
	// (the client's advertised caps intersected with what the proxy offered).
	Caps uint32
	// NextSeq is the packet sequence id the caller must use for the OK/ERR it
	// sends to conclude the handshake.
	NextSeq byte
}

// GreetClientMySQL performs the server side of the MySQL connection handshake
// against a connecting client. It sends a Handshake v10 advertising
// mysqlProxyClientCaps and a fresh salt, reads the client's HandshakeResponse41
// (which carries the target database name + username used for routing), and
// returns without writing the final OK packet. Authentication is trust: any
// password the client presents is accepted, so the salt/scramble are never
// checked here.
func GreetClientMySQL(conn io.ReadWriter, serverVersion string, connectionID uint32) (*ClientLogin, error) {
	salt, err := newMySQLSalt()
	if err != nil {
		return nil, err
	}
	greeting := EncodeHandshakeV10(
		serverVersion,
		connectionID,
		mysqlProxyClientCaps,
		DefaultCharsetUTF8MB4,
		serverStatusAutocommit,
		MySQLNativePassword,
		salt,
	)
	if _, err := WritePacket(conn, 0, greeting); err != nil {
		return nil, fmt.Errorf("write handshake: %w", err)
	}

	seq, payload, err := ReadPacket(conn)
	if err != nil {
		return nil, fmt.Errorf("read handshake response: %w", err)
	}
	resp, err := ParseHandshakeResponse41(payload)
	if err != nil {
		return nil, err
	}

	return &ClientLogin{
		Response: resp,
		Caps:     resp.Capabilities & mysqlProxyClientCaps,
		NextSeq:  seq + 1,
	}, nil
}
