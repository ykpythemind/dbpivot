package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/pem"
	"net"
	"testing"
	"time"
)

// fakeMySQLServer drives the server side of a handshake over conn. The
// behavior knobs let each test exercise a different auth path.
type fakeServerOpts struct {
	plugin   string // auth plugin advertised in the initial handshake
	salt     []byte // 20-byte nonce
	password string // the password the server expects (for native verification)

	// switchTo, when non-empty, makes the server respond to the first
	// HandshakeResponse41 with an AuthSwitchRequest to this plugin + switchSalt.
	switchTo   string
	switchSalt []byte

	// caching_sha2 control: when sha2FullAuth is true the server requests full
	// auth (0x04); otherwise it confirms fast auth (0x03). rsaKey, if set,
	// enables the public-key full-auth path (plaintext link).
	sha2FastFirst bool // send AuthMoreData 0x03 before OK (fast-auth success)
	sha2FullAuth  bool
	rsaKey        *rsa.PrivateKey

	// captured results for assertions
	gotResponse   *HandshakeResponse41
	gotCleartext  string
	gotRSAOK      bool
	authShouldErr bool // send ERR instead of OK at the end
}

func runFakeMySQLServer(t *testing.T, conn net.Conn, o *fakeServerOpts) {
	t.Helper()
	defer conn.Close()
	caps := uint32(ClientProtocol41 | ClientPluginAuth | ClientSecureConnection | ClientConnectWithDB)
	hs := EncodeHandshakeV10("8.0.40-fake", 7, caps, DefaultCharsetUTF8MB4, 0x0002, o.plugin, o.salt)
	seq, err := WritePacket(conn, 0, hs)
	if err != nil {
		t.Errorf("server WritePacket handshake: %v", err)
		return
	}

	rseq, payload, err := ReadPacket(conn)
	if err != nil {
		t.Errorf("server ReadPacket response: %v", err)
		return
	}
	resp, err := ParseHandshakeResponse41(payload)
	if err != nil {
		t.Errorf("server ParseHandshakeResponse41: %v", err)
		return
	}
	o.gotResponse = resp
	seq = rseq + 1

	plugin := o.plugin

	if o.switchTo != "" {
		// AuthSwitchRequest: 0xFE + plugin + NUL + salt + NUL
		sw := []byte{authSwitchRequestEOF}
		sw = append(sw, o.switchTo...)
		sw = append(sw, 0)
		sw = append(sw, o.switchSalt...)
		sw = append(sw, 0)
		if seq, err = WritePacket(conn, seq, sw); err != nil {
			t.Errorf("server write auth switch: %v", err)
			return
		}
		rseq, _, err = ReadPacket(conn) // switched auth response
		if err != nil {
			t.Errorf("server read switch response: %v", err)
			return
		}
		seq = rseq + 1
		plugin = o.switchTo
	}

	if plugin == CachingSHA2Password {
		if o.sha2FastFirst {
			if seq, err = WritePacket(conn, seq, []byte{authMoreDataHeader, cachingSHA2FastSuccess}); err != nil {
				t.Errorf("server write fast success: %v", err)
				return
			}
		} else if o.sha2FullAuth {
			if seq, err = WritePacket(conn, seq, []byte{authMoreDataHeader, cachingSHA2FullAuth}); err != nil {
				t.Errorf("server write full auth: %v", err)
				return
			}
			rseq, payload, err = ReadPacket(conn)
			if err != nil {
				t.Errorf("server read full-auth payload: %v", err)
				return
			}
			seq = rseq + 1
			if o.rsaKey != nil {
				// Expect a public-key request (0x02), respond with the PEM key.
				if len(payload) != 1 || payload[0] != cachingSHA2RequestKey {
					t.Errorf("server expected key request, got %x", payload)
					return
				}
				pubPEM := mustEncodePubPEM(t, &o.rsaKey.PublicKey)
				if seq, err = WritePacket(conn, seq, append([]byte{authMoreDataHeader}, pubPEM...)); err != nil {
					t.Errorf("server write pubkey: %v", err)
					return
				}
				rseq, payload, err = ReadPacket(conn)
				if err != nil {
					t.Errorf("server read encrypted password: %v", err)
					return
				}
				seq = rseq + 1
				dec, derr := rsa.DecryptOAEP(sha1.New(), rand.Reader, o.rsaKey, payload, nil)
				if derr != nil {
					t.Errorf("server decrypt: %v", derr)
					return
				}
				// XOR back with salt, strip trailing NUL.
				out := make([]byte, len(dec))
				for i := range dec {
					out[i] = dec[i] ^ o.salt[i%len(o.salt)]
				}
				out = bytes.TrimRight(out, "\x00")
				o.gotRSAOK = string(out) == o.password
			} else {
				// Cleartext over "TLS": password + NUL.
				o.gotCleartext = string(bytes.TrimRight(payload, "\x00"))
			}
		}
	}

	if o.authShouldErr {
		_, _ = WritePacket(conn, seq, EncodeERRPacket(1045, "28000", "Access denied"))
		return
	}
	_, _ = WritePacket(conn, seq, EncodeOKPacket(0, 0, 0x0002, 0))
}

func mustEncodePubPEM(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal pubkey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

func make20Salt(seed byte) []byte {
	s := make([]byte, 20)
	for i := range s {
		s[i] = seed + byte(i)
	}
	return s
}

// runAuth wires a client/server pipe, runs the fake server in a goroutine, and
// returns the AuthenticateUpstreamMySQL result.
func runAuth(t *testing.T, o *fakeServerOpts, user, password, database string, secure bool) (*HandshakeV10, uint32, error) {
	t.Helper()
	cConn, sConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		runFakeMySQLServer(t, sConn, o)
		close(done)
	}()
	type res struct {
		hs   *HandshakeV10
		caps uint32
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		hs, caps, err := AuthenticateUpstreamMySQL(cConn, user, password, database, secure)
		ch <- res{hs, caps, err}
	}()
	select {
	case r := <-ch:
		cConn.Close()
		<-done
		return r.hs, r.caps, r.err
	case <-time.After(3 * time.Second):
		cConn.Close()
		sConn.Close()
		t.Fatal("auth timed out")
		return nil, 0, nil
	}
}

func TestAuthenticateUpstreamMySQLNativePassword(t *testing.T) {
	o := &fakeServerOpts{plugin: MySQLNativePassword, salt: make20Salt(1), password: "s3cret"}
	hs, caps, err := runAuth(t, o, "appuser", "s3cret", "appdb", false)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if hs.ServerVersion != "8.0.40-fake" {
		t.Errorf("server version = %q", hs.ServerVersion)
	}
	if caps&ClientProtocol41 == 0 || caps&ClientConnectWithDB == 0 {
		t.Errorf("caps missing required bits: 0x%08x", caps)
	}
	if o.gotResponse.Username != "appuser" || o.gotResponse.Database != "appdb" {
		t.Errorf("server saw user=%q db=%q", o.gotResponse.Username, o.gotResponse.Database)
	}
	if !verifyNative(o.gotResponse.AuthResponse, o.salt, "s3cret") {
		t.Errorf("native scramble did not verify")
	}
}

func TestAuthenticateUpstreamMySQLNoDatabase(t *testing.T) {
	o := &fakeServerOpts{plugin: MySQLNativePassword, salt: make20Salt(3), password: ""}
	_, caps, err := runAuth(t, o, "u", "", "", false)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if caps&ClientConnectWithDB != 0 {
		t.Errorf("CLIENT_CONNECT_WITH_DB should be unset without a database")
	}
	if len(o.gotResponse.AuthResponse) != 0 {
		t.Errorf("empty password should yield empty auth response, got %x", o.gotResponse.AuthResponse)
	}
}

func TestAuthenticateUpstreamMySQLAccessDenied(t *testing.T) {
	o := &fakeServerOpts{plugin: MySQLNativePassword, salt: make20Salt(5), authShouldErr: true}
	_, _, err := runAuth(t, o, "u", "bad", "db", false)
	if err == nil {
		t.Fatal("expected auth error")
	}
}

func TestAuthenticateUpstreamMySQLAuthSwitch(t *testing.T) {
	// Server initially advertises caching_sha2 but switches the account to
	// native password with a fresh salt.
	o := &fakeServerOpts{
		plugin:     CachingSHA2Password,
		salt:       make20Salt(10),
		password:   "pw",
		switchTo:   MySQLNativePassword,
		switchSalt: make20Salt(99),
	}
	_, _, err := runAuth(t, o, "u", "pw", "db", false)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	// The fake server only captures the *first* response; verifying the switch
	// completed (no error) plus the OK is sufficient here.
}

func TestAuthenticateUpstreamMySQLCachingSHA2FastAuth(t *testing.T) {
	o := &fakeServerOpts{plugin: CachingSHA2Password, salt: make20Salt(20), password: "pw", sha2FastFirst: true}
	_, _, err := runAuth(t, o, "u", "pw", "db", false)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if !bytes.Equal(o.gotResponse.AuthResponse, ScrambleCachingSHA2Password("pw", o.salt)) {
		t.Errorf("caching_sha2 fast-auth scramble mismatch")
	}
}

func TestAuthenticateUpstreamMySQLCachingSHA2FullAuthCleartext(t *testing.T) {
	o := &fakeServerOpts{plugin: CachingSHA2Password, salt: make20Salt(30), password: "topsecret", sha2FullAuth: true}
	_, _, err := runAuth(t, o, "u", "topsecret", "db", true /* secure */)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if o.gotCleartext != "topsecret" {
		t.Errorf("server got cleartext %q, want %q", o.gotCleartext, "topsecret")
	}
}

func TestAuthenticateUpstreamMySQLCachingSHA2FullAuthRSA(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	o := &fakeServerOpts{
		plugin:       CachingSHA2Password,
		salt:         make20Salt(40),
		password:     "rsapw",
		sha2FullAuth: true,
		rsaKey:       key,
	}
	_, _, err = runAuth(t, o, "u", "rsapw", "db", false /* plaintext -> RSA */)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	if !o.gotRSAOK {
		t.Errorf("server failed to recover RSA-encrypted password")
	}
}

// verifyNative performs the server-side mysql_native_password check.
func verifyNative(proof, salt []byte, password string) bool {
	if password == "" {
		return len(proof) == 0
	}
	stage1 := sha1.Sum([]byte(password))
	token := sha1.Sum(stage1[:])
	check := sha1.Sum(append(append([]byte(nil), salt...), token[:]...))
	if len(proof) != len(check) {
		return false
	}
	recovered := make([]byte, len(proof))
	for i := range proof {
		recovered[i] = proof[i] ^ check[i]
	}
	return sha1.Sum(recovered) == token
}
