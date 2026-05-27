package proxy

import (
	"fmt"
	"net"

	"github.com/xdg-go/scram"
)

// AuthenticateUpstream completes the upstream-side SCRAM-SHA-256 exchange
// using user/password. It expects to start reading right after a
// StartupMessage has been sent on conn.
//
// v1 supports only SCRAM-SHA-256 (mechanism advertised first by the server)
// and the trivial AuthenticationOk case (server requires no authentication).
// Any other auth method results in an error.
func AuthenticateUpstream(conn net.Conn, user, password string) error {
	typ, body, err := ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read auth message: %w", err)
	}
	if typ != 'R' {
		if typ == 'E' {
			return fmt.Errorf("upstream rejected connection: %s", ParseErrorResponse(body))
		}
		return fmt.Errorf("expected Authentication message (R), got %c", typ)
	}
	code, data, err := ParseAuthenticationMessage(body)
	if err != nil {
		return err
	}

	switch code {
	case AuthOk:
		return nil
	case AuthSASL:
		return runSCRAM(conn, user, password, data)
	case AuthCleartextPassword, AuthMD5Password:
		return fmt.Errorf("upstream requires legacy auth (code %d); v1 supports SCRAM-SHA-256 only", code)
	default:
		return fmt.Errorf("unsupported upstream auth method (code %d)", code)
	}
}

// runSCRAM drives the SCRAM-SHA-256 conversation with the upstream.
// data is the body of the AuthenticationSASL message: a list of mechanism
// names separated by NULs and terminated by an additional NUL. We require
// SCRAM-SHA-256 to be advertised.
func runSCRAM(conn net.Conn, user, password string, data []byte) error {
	if !hasMechanism(data, "SCRAM-SHA-256") {
		return fmt.Errorf("upstream did not advertise SCRAM-SHA-256")
	}

	client, err := scram.SHA256.NewClient(user, password, "")
	if err != nil {
		return fmt.Errorf("scram client: %w", err)
	}
	conv := client.NewConversation()

	clientFirst, err := conv.Step("")
	if err != nil {
		return fmt.Errorf("scram client-first: %w", err)
	}
	if err := WriteSASLInitialResponse(conn, "SCRAM-SHA-256", clientFirst); err != nil {
		return fmt.Errorf("write SASLInitialResponse: %w", err)
	}

	// Expect AuthenticationSASLContinue.
	serverFirst, err := readSASLContinue(conn)
	if err != nil {
		return err
	}
	clientFinal, err := conv.Step(serverFirst)
	if err != nil {
		return fmt.Errorf("scram client-final: %w", err)
	}
	if err := WriteSASLResponse(conn, clientFinal); err != nil {
		return fmt.Errorf("write SASLResponse: %w", err)
	}

	// Expect AuthenticationSASLFinal.
	serverFinal, err := readSASLFinal(conn)
	if err != nil {
		return err
	}
	if _, err := conv.Step(serverFinal); err != nil {
		return fmt.Errorf("scram verify: %w", err)
	}
	if !conv.Valid() {
		return fmt.Errorf("scram conversation did not complete successfully")
	}

	// Then AuthenticationOk.
	typ, body, err := ReadMessage(conn)
	if err != nil {
		return fmt.Errorf("read AuthenticationOk: %w", err)
	}
	if typ != 'R' {
		return fmt.Errorf("expected AuthenticationOk, got %c", typ)
	}
	code, _, err := ParseAuthenticationMessage(body)
	if err != nil {
		return err
	}
	if code != AuthOk {
		return fmt.Errorf("expected AuthenticationOk code 0, got %d", code)
	}
	return nil
}

func readSASLContinue(conn net.Conn) (string, error) {
	typ, body, err := ReadMessage(conn)
	if err != nil {
		return "", fmt.Errorf("read SASLContinue: %w", err)
	}
	if typ != 'R' {
		return "", fmt.Errorf("expected 'R' for SASLContinue, got %c", typ)
	}
	code, data, err := ParseAuthenticationMessage(body)
	if err != nil {
		return "", err
	}
	if code != AuthSASLContinue {
		return "", fmt.Errorf("expected AuthSASLContinue (11), got %d", code)
	}
	return string(data), nil
}

func readSASLFinal(conn net.Conn) (string, error) {
	typ, body, err := ReadMessage(conn)
	if err != nil {
		return "", fmt.Errorf("read SASLFinal: %w", err)
	}
	if typ != 'R' {
		return "", fmt.Errorf("expected 'R' for SASLFinal, got %c", typ)
	}
	code, data, err := ParseAuthenticationMessage(body)
	if err != nil {
		return "", err
	}
	if code != AuthSASLFinal {
		return "", fmt.Errorf("expected AuthSASLFinal (12), got %d", code)
	}
	return string(data), nil
}

// hasMechanism checks whether the NUL-separated list in data contains name.
func hasMechanism(data []byte, name string) bool {
	start := 0
	for i, b := range data {
		if b == 0 {
			if string(data[start:i]) == name {
				return true
			}
			start = i + 1
		}
	}
	return false
}
