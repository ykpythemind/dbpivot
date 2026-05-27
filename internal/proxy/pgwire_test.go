package proxy

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestParseErrorResponse(t *testing.T) {
	// Field order: type byte + NUL-terminated value, terminated by a zero byte.
	body := []byte("SFATAL\x00C28000\x00Mno pg_hba.conf entry for host \"x\", no encryption\x00\x00")
	got := ParseErrorResponse(body)
	want := `FATAL: no pg_hba.conf entry for host "x", no encryption (SQLSTATE 28000)`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseErrorResponse_Empty(t *testing.T) {
	if got := ParseErrorResponse([]byte{0}); got != "unparseable ErrorResponse" {
		t.Errorf("got %q", got)
	}
}

func TestParseStartupBody_OK(t *testing.T) {
	body := []byte("user\x00alice\x00database\x00app\x00\x00")
	got, err := ParseStartupBody(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := []KV{{"user", "alice"}, {"database", "app"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestParseStartupBody_Malformed(t *testing.T) {
	cases := map[string][]byte{
		"no_trailing_nul": []byte("user\x00alice\x00"),
		"empty_key":       []byte("\x00val\x00\x00"),
		"missing_val":     []byte("user\x00"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseStartupBody(body); err == nil {
				t.Errorf("expected error for %s", name)
			}
		})
	}
}

func TestEncodeStartup_Roundtrip(t *testing.T) {
	params := []KV{{"user", "alice"}, {"database", "appdb"}, {"application_name", "psql"}}
	encoded := EncodeStartup(params)

	length := binary.BigEndian.Uint32(encoded[0:4])
	if int(length) != len(encoded) {
		t.Errorf("length prefix %d != actual %d", length, len(encoded))
	}
	if proto := binary.BigEndian.Uint32(encoded[4:8]); proto != ProtocolV3 {
		t.Errorf("proto = 0x%x, want 0x%x", proto, ProtocolV3)
	}
	got, err := ParseStartupBody(encoded[8:])
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !reflect.DeepEqual(got, params) {
		t.Errorf("roundtrip got %v, want %v", got, params)
	}
}

func TestUpsertParam(t *testing.T) {
	p := []KV{{"user", "alice"}, {"database", "app_dev"}}
	p = UpsertParam(p, "database", "app_main_staging")
	if p[1].V != "app_main_staging" {
		t.Errorf("replace failed: %v", p)
	}

	p = UpsertParam(p, "application_name", "psql")
	if len(p) != 3 || p[2].K != "application_name" || p[2].V != "psql" {
		t.Errorf("append failed: %v", p)
	}
}

func TestWriteAuthenticationOk(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteAuthenticationOk(&buf); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if len(got) != 9 {
		t.Fatalf("len = %d, want 9", len(got))
	}
	if got[0] != 'R' {
		t.Errorf("type = %c", got[0])
	}
	if binary.BigEndian.Uint32(got[1:5]) != 8 {
		t.Errorf("len prefix wrong")
	}
	if binary.BigEndian.Uint32(got[5:9]) != AuthOk {
		t.Errorf("code = %d", binary.BigEndian.Uint32(got[5:9]))
	}
}

func TestWriteErrorResponse(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteErrorResponse(&buf, "FATAL", "3D000", `database "foo" not configured`); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if got[0] != 'E' {
		t.Fatalf("type = %c", got[0])
	}
	length := binary.BigEndian.Uint32(got[1:5])
	if int(length)+1 != len(got) {
		t.Errorf("length mismatch: prefix=%d total=%d", length, len(got))
	}
	body := got[5:]
	if body[len(body)-1] != 0 {
		t.Errorf("body must end in NUL")
	}
	if !bytes.Contains(body, []byte("FATAL\x00")) {
		t.Errorf("missing severity")
	}
	if !bytes.Contains(body, []byte("3D000\x00")) {
		t.Errorf("missing sqlstate")
	}
	if !bytes.Contains(body, []byte(`database "foo" not configured`)) {
		t.Errorf("missing message")
	}
}

func TestWriteSASLInitialResponse(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteSASLInitialResponse(&buf, "SCRAM-SHA-256", "n,,n=user,r=abc"); err != nil {
		t.Fatal(err)
	}
	got := buf.Bytes()
	if got[0] != 'p' {
		t.Fatalf("type = %c", got[0])
	}
	body := got[5:]
	nul := bytes.IndexByte(body, 0)
	if nul < 0 {
		t.Fatal("no NUL after mechanism")
	}
	if string(body[:nul]) != "SCRAM-SHA-256" {
		t.Errorf("mechanism = %q", body[:nul])
	}
	dataLen := binary.BigEndian.Uint32(body[nul+1 : nul+5])
	if int(dataLen) != len("n,,n=user,r=abc") {
		t.Errorf("data length = %d", dataLen)
	}
	if string(body[nul+5:]) != "n,,n=user,r=abc" {
		t.Errorf("data = %q", body[nul+5:])
	}
}

func TestReadWriteMessage(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteMessage(&buf, 'X', []byte("hello")); err != nil {
		t.Fatal(err)
	}
	typ, body, err := ReadMessage(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if typ != 'X' || string(body) != "hello" {
		t.Errorf("got typ=%c body=%q", typ, body)
	}
}
