package proxy

import (
	"net"
	"testing"
	"time"
)

// fakeMySQLClient drives the client side of the handshake against
// GreetClientMySQL: it reads the proxy's greeting, sends a HandshakeResponse41
// built from the knobs below, then (optionally) reads the trailing OK packet
// the caller writes.
type fakeClientOpts struct {
	username string
	password string
	database string
	// extraCaps are OR'd onto the protocol-mandatory caps the client advertises.
	extraCaps uint32

	// captured for assertions
	gotGreeting *HandshakeV10
}

func runFakeMySQLClient(t *testing.T, conn net.Conn, o *fakeClientOpts) {
	t.Helper()
	defer conn.Close()

	_, payload, err := ReadPacket(conn)
	if err != nil {
		t.Errorf("client read greeting: %v", err)
		return
	}
	hs, err := ParseHandshakeV10(payload)
	if err != nil {
		t.Errorf("client ParseHandshakeV10: %v", err)
		return
	}
	o.gotGreeting = hs

	caps := uint32(ClientProtocol41 | ClientSecureConnection | ClientPluginAuth)
	caps |= o.extraCaps
	if o.database != "" {
		caps |= ClientConnectWithDB
	}
	plugin := hs.AuthPluginName
	if plugin == "" {
		plugin = MySQLNativePassword
	}
	authResp := ScrambleNativePassword(o.password, hs.AuthPluginData)
	resp := EncodeHandshakeResponse41(caps, DefaultCharsetUTF8MB4, o.username, authResp, o.database, plugin)
	if _, err := WritePacket(conn, 1, resp); err != nil {
		t.Errorf("client write handshake response: %v", err)
		return
	}

	// The proxy concludes by sending an OK packet (written by the test body
	// after GreetClientMySQL returns); read and verify it is an OK.
	_, okPayload, err := ReadPacket(conn)
	if err != nil {
		t.Errorf("client read OK: %v", err)
		return
	}
	if !IsOKPacket(okPayload) {
		t.Errorf("client: expected OK packet, got header 0x%02x", firstByte(okPayload))
	}
}

func TestGreetClientMySQL(t *testing.T) {
	tests := []struct {
		name     string
		opts     fakeClientOpts
		wantDB   string
		wantUser string
	}{
		{
			name:     "with database and password",
			opts:     fakeClientOpts{username: "appuser", password: "s3cret", database: "shop"},
			wantDB:   "shop",
			wantUser: "appuser",
		},
		{
			name:     "no database, empty password (trust)",
			opts:     fakeClientOpts{username: "root", password: ""},
			wantDB:   "",
			wantUser: "root",
		},
		{
			name:     "client advertises lenenc client data + deprecate eof",
			opts:     fakeClientOpts{username: "u", password: "p", database: "db", extraCaps: ClientPluginAuthLenencClientData | ClientDeprecateEOF},
			wantDB:   "db",
			wantUser: "u",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clientConn, proxyConn := net.Pipe()
			_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
			_ = proxyConn.SetDeadline(time.Now().Add(5 * time.Second))

			done := make(chan struct{})
			go func() {
				defer close(done)
				runFakeMySQLClient(t, clientConn, &tc.opts)
			}()

			login, err := GreetClientMySQL(proxyConn, "8.0.0-dbpivot", 42)
			if err != nil {
				t.Fatalf("GreetClientMySQL: %v", err)
			}
			if login.Response.Username != tc.wantUser {
				t.Errorf("username = %q, want %q", login.Response.Username, tc.wantUser)
			}
			if login.Response.Database != tc.wantDB {
				t.Errorf("database = %q, want %q", login.Response.Database, tc.wantDB)
			}
			// Negotiated caps must be a subset of what the proxy advertised.
			if login.Caps&^uint32(mysqlProxyClientCaps) != 0 {
				t.Errorf("negotiated caps 0x%08x exceed advertised 0x%08x", login.Caps, uint32(mysqlProxyClientCaps))
			}
			// The proxy must have advertised mysql_native_password as default.
			if tc.opts.gotGreeting != nil && tc.opts.gotGreeting.AuthPluginName != MySQLNativePassword {
				t.Errorf("greeting auth plugin = %q, want %q", tc.opts.gotGreeting.AuthPluginName, MySQLNativePassword)
			}

			// Conclude the handshake with an OK so the fake client unblocks.
			if _, err := WritePacket(proxyConn, login.NextSeq, EncodeOKPacket(0, 0, serverStatusAutocommit, 0)); err != nil {
				t.Fatalf("write OK: %v", err)
			}
			<-done
		})
	}
}

// TestGreetClientMySQL_AdvertisesNoSSL guards the plaintext-trust invariant:
// the greeting must not advertise CLIENT_SSL, otherwise a client could try to
// upgrade the (unsupported) client→proxy TLS leg.
func TestGreetClientMySQL_AdvertisesNoSSL(t *testing.T) {
	if mysqlProxyClientCaps&ClientSSL != 0 {
		t.Fatalf("mysqlProxyClientCaps must not advertise CLIENT_SSL")
	}

	clientConn, proxyConn := net.Pipe()
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = proxyConn.SetDeadline(time.Now().Add(5 * time.Second))

	done := make(chan struct{})
	var greeting *HandshakeV10
	go func() {
		defer close(done)
		defer clientConn.Close()
		_, payload, err := ReadPacket(clientConn)
		if err != nil {
			t.Errorf("client read greeting: %v", err)
			return
		}
		greeting, err = ParseHandshakeV10(payload)
		if err != nil {
			t.Errorf("ParseHandshakeV10: %v", err)
		}
	}()

	// GreetClientMySQL will block reading the response; we only care about the
	// greeting it sent, so close the proxy side once the client has read it.
	go func() {
		_, _ = GreetClientMySQL(proxyConn, "8.0.0-dbpivot", 1)
	}()
	<-done
	_ = proxyConn.Close()

	if greeting == nil {
		t.Fatal("no greeting captured")
	}
	if greeting.Capabilities&ClientSSL != 0 {
		t.Errorf("greeting advertised CLIENT_SSL (caps 0x%08x)", greeting.Capabilities)
	}
}
