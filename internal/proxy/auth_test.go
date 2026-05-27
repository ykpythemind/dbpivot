package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/xdg-go/scram"
)

func TestHasMechanism(t *testing.T) {
	data := []byte("SCRAM-SHA-256\x00SCRAM-SHA-256-PLUS\x00\x00")
	if !hasMechanism(data, "SCRAM-SHA-256") {
		t.Error("should find SCRAM-SHA-256")
	}
	if !hasMechanism(data, "SCRAM-SHA-256-PLUS") {
		t.Error("should find SCRAM-SHA-256-PLUS")
	}
	if hasMechanism(data, "GSSAPI") {
		t.Error("should not find GSSAPI")
	}
}

func TestAuthenticateUpstream_AuthOk(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		done <- AuthenticateUpstream(a, "user", "pass")
	}()

	var body [4]byte
	binary.BigEndian.PutUint32(body[:], AuthOk)
	if err := WriteMessage(b, 'R', body[:]); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAuthenticateUpstream_ErrorResponse(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		done <- AuthenticateUpstream(a, "user", "pass")
	}()

	body := []byte("SFATAL\x00C28000\x00Mno pg_hba.conf entry, no encryption\x00\x00")
	if err := WriteMessage(b, 'E', body); err != nil {
		t.Fatal(err)
	}

	err := <-done
	if err == nil || !strings.Contains(err.Error(), "no encryption") || !strings.Contains(err.Error(), "28000") {
		t.Errorf("expected surfaced upstream error, got %v", err)
	}
}

func TestNegotiateUpstreamTLS_ServerDeclines(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		_, err := NegotiateUpstreamTLS(a, "example")
		done <- err
	}()

	// Drain the 8-byte SSLRequest the client sends, then decline with 'N'.
	var req [8]byte
	if _, err := io.ReadFull(b, req[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte{'N'}); err != nil {
		t.Fatal(err)
	}

	err := <-done
	if err == nil || !strings.Contains(err.Error(), "does not support SSL") {
		t.Errorf("expected SSL-unsupported error, got %v", err)
	}
}

func TestAuthenticateUpstream_LegacyRejected(t *testing.T) {
	cases := []struct {
		name string
		code uint32
	}{
		{"cleartext", AuthCleartextPassword},
		{"md5", AuthMD5Password},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, b := net.Pipe()
			defer b.Close()

			done := make(chan error, 1)
			go func() {
				done <- AuthenticateUpstream(a, "user", "pass")
			}()

			body := make([]byte, 4)
			binary.BigEndian.PutUint32(body, c.code)
			if c.code == AuthMD5Password {
				body = append(body, 1, 2, 3, 4)
			}
			if err := WriteMessage(b, 'R', body); err != nil {
				t.Fatal(err)
			}

			err := <-done
			if err == nil || !strings.Contains(err.Error(), "legacy auth") {
				t.Errorf("expected legacy auth error, got %v", err)
			}
		})
	}
}

func TestAuthenticateUpstream_SCRAMHappyPath(t *testing.T) {
	const user = "scramtest"
	const password = "secret"

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		done <- AuthenticateUpstream(a, user, password)
	}()

	if err := runFakeSCRAMServer(b, user, password); err != nil {
		t.Fatalf("fake server: %v", err)
	}

	if err := <-done; err != nil {
		t.Fatalf("AuthenticateUpstream: %v", err)
	}
}

func TestAuthenticateUpstream_SCRAMBadPassword(t *testing.T) {
	const user = "scramtest"

	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	done := make(chan error, 1)
	go func() {
		done <- AuthenticateUpstream(a, user, "wrongpass")
	}()

	_ = runFakeSCRAMServer(b, user, "rightpass")
	b.Close()

	err := <-done
	if err == nil {
		t.Fatal("expected error for bad password")
	}
}

func runFakeSCRAMServer(conn net.Conn, user, password string) error {
	// 1. Send AuthenticationSASL advertising SCRAM-SHA-256.
	body := make([]byte, 4)
	binary.BigEndian.PutUint32(body, AuthSASL)
	body = append(body, "SCRAM-SHA-256"...)
	body = append(body, 0, 0)
	if err := WriteMessage(conn, 'R', body); err != nil {
		return err
	}

	// 2. Receive SASLInitialResponse ('p').
	typ, frame, err := ReadMessage(conn)
	if err != nil {
		return err
	}
	if typ != 'p' {
		return fmt.Errorf("expected 'p', got %c", typ)
	}
	nul := indexByte(frame, 0, 0)
	if nul < 0 {
		return fmt.Errorf("missing mechanism terminator")
	}
	if string(frame[:nul]) != "SCRAM-SHA-256" {
		return fmt.Errorf("unexpected mechanism %q", frame[:nul])
	}
	dataLen := binary.BigEndian.Uint32(frame[nul+1 : nul+5])
	clientFirst := string(frame[nul+5 : nul+5+int(dataLen)])

	// 3. Build credentials and run a SCRAM server conversation.
	cli, err := scram.SHA256.NewClient(user, password, "")
	if err != nil {
		return err
	}
	kf := scram.KeyFactors{Iters: 4096, Salt: "salty-mc-salt"}
	stored, err := cli.GetStoredCredentialsWithError(kf)
	if err != nil {
		return err
	}
	kfLookup := func(string) (scram.StoredCredentials, error) { return stored, nil }
	server, err := scram.SHA256.NewServer(kfLookup)
	if err != nil {
		return err
	}
	conv := server.NewConversation()

	serverFirst, err := conv.Step(clientFirst)
	if err != nil {
		return err
	}
	cbody := make([]byte, 4)
	binary.BigEndian.PutUint32(cbody, AuthSASLContinue)
	cbody = append(cbody, serverFirst...)
	if err := WriteMessage(conn, 'R', cbody); err != nil {
		return err
	}

	typ, frame, err = ReadMessage(conn)
	if err != nil {
		return err
	}
	if typ != 'p' {
		return fmt.Errorf("expected 'p', got %c", typ)
	}
	clientFinal := string(frame)

	serverFinal, err := conv.Step(clientFinal)
	if err != nil {
		return err
	}
	fbody := make([]byte, 4)
	binary.BigEndian.PutUint32(fbody, AuthSASLFinal)
	fbody = append(fbody, serverFinal...)
	if err := WriteMessage(conn, 'R', fbody); err != nil {
		return err
	}

	okBody := make([]byte, 4)
	binary.BigEndian.PutUint32(okBody, AuthOk)
	return WriteMessage(conn, 'R', okBody)
}
