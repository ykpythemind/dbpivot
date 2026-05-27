package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
)

// MySQL upstream authentication driver.
//
// This mirrors auth.go's AuthenticateUpstream (the PostgreSQL SCRAM path) but
// speaks the MySQL handshake from the *client* side: the proxy has just dialed
// an upstream MySQL server, reads its initial Handshake v10, and logs in with
// the configured user/password/database. After this returns, the command phase
// can be piped through unchanged.
//
// Supported auth plugins (matching the v1 scope of "common deployments"):
//   - mysql_native_password (MySQL 5.7 / MariaDB default)
//   - caching_sha2_password (MySQL 8.0 default): fast-auth, full-auth over a
//     secure (TLS) link via cleartext, and full-auth over plaintext via the
//     server's RSA public key.
//
// AuthSwitchRequest is honored, so a server defaulting to one plugin while the
// account uses another still authenticates.

// AuthMoreData / AuthSwitchRequest packet headers (command/handshake phase).
const (
	authMoreDataHeader     = 0x01
	authSwitchRequestEOF   = 0xFE
	cachingSHA2FastSuccess = 0x03
	cachingSHA2FullAuth    = 0x04
	cachingSHA2RequestKey  = 0x02
)

// mysqlUpstreamCaps computes the capability flags the proxy advertises to the
// upstream. It keeps a conservative, broadly-supported set intersected with
// what the server offers, plus CLIENT_CONNECT_WITH_DB when a database is given.
func mysqlUpstreamCaps(serverCaps uint32, withDB bool) uint32 {
	desired := uint32(ClientLongPassword |
		ClientLongFlag |
		ClientProtocol41 |
		ClientTransactions |
		ClientSecureConnection |
		ClientPluginAuth |
		ClientPluginAuthLenencClientData |
		ClientDeprecateEOF)
	caps := desired & serverCaps
	// These are mandatory for a modern login regardless of the server's
	// advertised bits (every 4.1+ server honors them).
	caps |= ClientProtocol41 | ClientSecureConnection | ClientPluginAuth
	if withDB {
		caps |= ClientConnectWithDB
	}
	return caps
}

// mysqlAuthResponse computes the initial auth response for the given plugin.
func mysqlAuthResponse(plugin, password string, salt []byte) ([]byte, error) {
	switch plugin {
	case MySQLNativePassword, "":
		return ScrambleNativePassword(password, salt), nil
	case CachingSHA2Password:
		return ScrambleCachingSHA2Password(password, salt), nil
	default:
		return nil, fmt.Errorf("unsupported auth plugin %q", plugin)
	}
}

// AuthenticateUpstreamMySQL reads the upstream's initial handshake, logs in
// with user/password/database, and drives any auth-switch / caching_sha2
// follow-ups to completion. secure reports whether conn is already encrypted
// (TLS), which decides how caching_sha2 full-auth sends the password.
//
// It returns the parsed handshake and the capability flags the proxy
// negotiated with the upstream, so the caller can keep both legs consistent.
func AuthenticateUpstreamMySQL(conn io.ReadWriter, user, password, database string, secure bool) (*HandshakeV10, uint32, error) {
	seq, payload, err := ReadPacket(conn)
	if err != nil {
		return nil, 0, fmt.Errorf("read handshake: %w", err)
	}
	if IsErrPacket(payload) {
		return nil, 0, fmt.Errorf("upstream rejected connection: %s", ParseERRPacket(payload))
	}
	hs, err := ParseHandshakeV10(payload)
	if err != nil {
		return nil, 0, err
	}

	plugin := hs.AuthPluginName
	if plugin == "" {
		plugin = MySQLNativePassword
	}
	caps := mysqlUpstreamCaps(hs.Capabilities, database != "")

	authResp, err := mysqlAuthResponse(plugin, password, hs.AuthPluginData)
	if err != nil {
		return nil, 0, err
	}

	resp := EncodeHandshakeResponse41(caps, DefaultCharsetUTF8MB4, user, authResp, database, plugin)
	seq++
	if seq, err = WritePacket(conn, seq, resp); err != nil {
		return nil, 0, fmt.Errorf("write handshake response: %w", err)
	}

	if err := mysqlFinishAuth(conn, seq, plugin, password, hs.AuthPluginData, secure); err != nil {
		return nil, 0, err
	}
	return hs, caps, nil
}

// mysqlFinishAuth consumes server packets after the HandshakeResponse41 until
// the login is accepted (OK) or rejected (ERR / error). seq is the next
// sequence id to use for any packet the proxy sends. salt is the nonce the
// current plugin scrambles against (updated on an auth switch).
func mysqlFinishAuth(conn io.ReadWriter, seq byte, plugin, password string, salt []byte, secure bool) error {
	for {
		rseq, payload, err := ReadPacket(conn)
		if err != nil {
			return fmt.Errorf("read auth result: %w", err)
		}
		seq = rseq + 1

		if len(payload) == 0 {
			return fmt.Errorf("empty auth packet")
		}
		switch payload[0] {
		case OKPacketHeader:
			return nil
		case ErrPacketHeader:
			return fmt.Errorf("upstream auth failed: %s", ParseERRPacket(payload))
		case authSwitchRequestEOF:
			// AuthSwitchRequest: 0xFE, plugin name (NUL), auth data.
			newPlugin, np, perr := readNulString(payload, 1)
			if perr != nil {
				return fmt.Errorf("auth switch: %w", perr)
			}
			newSalt := bytes.TrimRight(payload[np:], "\x00")
			plugin = newPlugin
			salt = newSalt
			authResp, aerr := mysqlAuthResponse(plugin, password, salt)
			if aerr != nil {
				return aerr
			}
			if seq, err = WritePacket(conn, seq, authResp); err != nil {
				return fmt.Errorf("write auth switch response: %w", err)
			}
		case authMoreDataHeader:
			// caching_sha2_password follow-up.
			if plugin != CachingSHA2Password {
				return fmt.Errorf("unexpected AuthMoreData for plugin %q", plugin)
			}
			if err := mysqlCachingSHA2More(conn, &seq, payload, password, salt, secure); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unexpected auth packet header 0x%02x", payload[0])
		}
	}
}

// mysqlCachingSHA2More handles a caching_sha2_password AuthMoreData packet:
// either a fast-auth success marker (then the OK follows on the next read) or a
// full-auth request, which the proxy satisfies by sending the password — in
// cleartext over TLS, or RSA-encrypted via the server's public key otherwise.
func mysqlCachingSHA2More(conn io.ReadWriter, seq *byte, payload []byte, password string, salt []byte, secure bool) error {
	if len(payload) < 2 {
		return fmt.Errorf("short caching_sha2 AuthMoreData")
	}
	switch payload[1] {
	case cachingSHA2FastSuccess:
		// Fast auth confirmed; the OK packet is read by the outer loop.
		return nil
	case cachingSHA2FullAuth:
		if password == "" {
			// No password: send a single NUL terminator.
			next, err := WritePacket(conn, *seq, []byte{0})
			*seq = next
			return err
		}
		if secure {
			// Over TLS the password is sent in cleartext (NUL-terminated).
			next, err := WritePacket(conn, *seq, append([]byte(password), 0))
			*seq = next
			return err
		}
		return mysqlCachingSHA2RSA(conn, seq, password, salt)
	default:
		return fmt.Errorf("unexpected caching_sha2 marker 0x%02x", payload[1])
	}
}

// mysqlCachingSHA2RSA performs caching_sha2_password full auth over a plaintext
// link: request the server's RSA public key, then send the password XOR-scrambled
// with the nonce and RSA-OAEP encrypted under that key.
func mysqlCachingSHA2RSA(conn io.ReadWriter, seq *byte, password string, salt []byte) error {
	// Request the public key (0x02).
	next, err := WritePacket(conn, *seq, []byte{cachingSHA2RequestKey})
	if err != nil {
		return fmt.Errorf("request public key: %w", err)
	}
	*seq = next

	rseq, payload, err := ReadPacket(conn)
	if err != nil {
		return fmt.Errorf("read public key: %w", err)
	}
	*seq = rseq + 1
	if len(payload) < 1 || payload[0] != authMoreDataHeader {
		return fmt.Errorf("expected public key AuthMoreData, got 0x%02x", firstByte(payload))
	}
	pub, err := parseRSAPublicKey(payload[1:])
	if err != nil {
		return err
	}

	enc, err := encryptCachingSHA2Password(password, salt, pub)
	if err != nil {
		return err
	}
	next, err = WritePacket(conn, *seq, enc)
	if err != nil {
		return fmt.Errorf("write encrypted password: %w", err)
	}
	*seq = next
	return nil
}

// encryptCachingSHA2Password XORs the NUL-terminated password with the repeated
// nonce and RSA-OAEP(SHA-1) encrypts it under pub, matching libmysql's
// caching_sha2 full-auth client behavior.
func encryptCachingSHA2Password(password string, salt []byte, pub *rsa.PublicKey) ([]byte, error) {
	if len(salt) == 0 {
		return nil, fmt.Errorf("caching_sha2 RSA: empty nonce")
	}
	plain := append([]byte(password), 0)
	obf := make([]byte, len(plain))
	for i := range plain {
		obf[i] = plain[i] ^ salt[i%len(salt)]
	}
	return rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, obf, nil)
}

// parseRSAPublicKey decodes a PEM-encoded RSA public key (PKIX SubjectPublicKeyInfo,
// the form MySQL hands out).
func parseRSAPublicKey(pemData []byte) (*rsa.PublicKey, error) {
	block, _ := pem.Decode(bytes.TrimRight(pemData, "\x00"))
	if block == nil {
		return nil, fmt.Errorf("caching_sha2 RSA: invalid PEM public key")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("caching_sha2 RSA: parse public key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("caching_sha2 RSA: not an RSA public key")
	}
	return rsaKey, nil
}

func firstByte(b []byte) byte {
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
