package proxy

import (
	"bytes"
	"crypto/sha1"
	"reflect"
	"testing"
)

func TestLenencIntRoundTrip(t *testing.T) {
	cases := []uint64{0, 1, 0xFA, 0xFB, 0xFF, 0x100, 0xFFFF, 0x10000, 0xFFFFFF, 0x1000000, 0xFFFFFFFFFFFFFFFF}
	for _, want := range cases {
		b := AppendLenencInt(nil, want)
		got, next, err := ReadLenencInt(b, 0)
		if err != nil {
			t.Fatalf("ReadLenencInt(%d): %v", want, err)
		}
		if got != want {
			t.Errorf("lenenc %d: got %d", want, got)
		}
		if next != len(b) {
			t.Errorf("lenenc %d: next=%d, want %d", want, next, len(b))
		}
	}
}

func TestPacketRoundTrip(t *testing.T) {
	payload := []byte("hello mysql packet")
	var buf bytes.Buffer
	next, err := WritePacket(&buf, 0, payload)
	if err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if next != 1 {
		t.Errorf("next seq = %d, want 1", next)
	}
	seq, got, err := ReadPacket(&buf)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if seq != 0 {
		t.Errorf("seq = %d, want 0", seq)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("payload = %q, want %q", got, payload)
	}
}

func TestHandshakeV10RoundTrip(t *testing.T) {
	salt := make([]byte, 20)
	for i := range salt {
		salt[i] = byte(i + 1)
	}
	caps := uint32(ClientProtocol41 | ClientPluginAuth | ClientSecureConnection | ClientConnectWithDB)
	enc := EncodeHandshakeV10("8.0.40-dbpivot", 42, caps, DefaultCharsetUTF8MB4, 0x0002, MySQLNativePassword, salt)
	h, err := ParseHandshakeV10(enc)
	if err != nil {
		t.Fatalf("ParseHandshakeV10: %v", err)
	}
	if h.ServerVersion != "8.0.40-dbpivot" {
		t.Errorf("server version = %q", h.ServerVersion)
	}
	if h.ConnectionID != 42 {
		t.Errorf("connection id = %d", h.ConnectionID)
	}
	if h.Capabilities != caps {
		t.Errorf("capabilities = 0x%08x, want 0x%08x", h.Capabilities, caps)
	}
	if h.AuthPluginName != MySQLNativePassword {
		t.Errorf("auth plugin = %q", h.AuthPluginName)
	}
	if !bytes.Equal(h.AuthPluginData, salt) {
		t.Errorf("salt = %x, want %x", h.AuthPluginData, salt)
	}
}

func TestHandshakeResponse41RoundTrip(t *testing.T) {
	caps := uint32(ClientProtocol41 | ClientPluginAuth | ClientSecureConnection | ClientConnectWithDB)
	auth := []byte("0123456789abcdef0123") // 20 bytes
	enc := EncodeHandshakeResponse41(caps, DefaultCharsetUTF8MB4, "appuser", auth, "appdb", MySQLNativePassword)
	r, err := ParseHandshakeResponse41(enc)
	if err != nil {
		t.Fatalf("ParseHandshakeResponse41: %v", err)
	}
	if r.Username != "appuser" {
		t.Errorf("username = %q", r.Username)
	}
	if r.Database != "appdb" {
		t.Errorf("database = %q", r.Database)
	}
	if r.AuthPlugin != MySQLNativePassword {
		t.Errorf("auth plugin = %q", r.AuthPlugin)
	}
	if !bytes.Equal(r.AuthResponse, auth) {
		t.Errorf("auth response = %x, want %x", r.AuthResponse, auth)
	}
	if r.Capabilities != caps {
		t.Errorf("capabilities = 0x%08x", r.Capabilities)
	}
}

func TestHandshakeResponse41NoDB(t *testing.T) {
	caps := uint32(ClientProtocol41 | ClientPluginAuth | ClientSecureConnection)
	enc := EncodeHandshakeResponse41(caps, DefaultCharsetUTF8MB4, "u", []byte{1, 2, 3}, "", MySQLNativePassword)
	r, err := ParseHandshakeResponse41(enc)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if r.Database != "" {
		t.Errorf("database = %q, want empty", r.Database)
	}
	if r.Username != "u" {
		t.Errorf("username = %q", r.Username)
	}
}

func TestEncodeParseERRPacket(t *testing.T) {
	enc := EncodeERRPacket(1045, "28000", "Access denied for user 'x'")
	if !IsErrPacket(enc) {
		t.Fatal("IsErrPacket = false")
	}
	got := ParseERRPacket(enc)
	want := "ERROR 1045 (28000): Access denied for user 'x'"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestEncodeOKPacket(t *testing.T) {
	enc := EncodeOKPacket(0, 0, 0x0002, 0)
	if !IsOKPacket(enc) {
		t.Errorf("IsOKPacket = false for %x", enc)
	}
}

// TestScrambleNativePassword verifies the scramble by performing the
// server-side check: given the stored token SHA1(SHA1(password)) and the salt,
// the server recovers SHA1(password) from the client proof and confirms it
// hashes to the stored token.
func TestScrambleNativePassword(t *testing.T) {
	const password = "s3cr3t-pw"
	salt := []byte("12345678901234567890") // 20 bytes

	proof := ScrambleNativePassword(password, salt)
	if len(proof) != sha1.Size {
		t.Fatalf("proof len = %d, want %d", len(proof), sha1.Size)
	}

	stage1 := sha1.Sum([]byte(password)) // SHA1(password)
	token := sha1.Sum(stage1[:])         // SHA1(SHA1(password)) — what the server stores
	check := sha1.Sum(append(append([]byte(nil), salt...), token[:]...))

	recovered := make([]byte, sha1.Size)
	for i := range proof {
		recovered[i] = proof[i] ^ check[i]
	}
	if !bytes.Equal(recovered, stage1[:]) {
		t.Errorf("recovered SHA1(password) = %x, want %x", recovered, stage1[:])
	}
	// And the recovered stage1 must hash to the stored token.
	again := sha1.Sum(recovered)
	if again != token {
		t.Errorf("server verification failed")
	}
}

func TestScrambleEmptyPassword(t *testing.T) {
	if got := ScrambleNativePassword("", []byte("12345678901234567890")); got != nil {
		t.Errorf("empty password scramble = %x, want nil", got)
	}
	if got := ScrambleCachingSHA2Password("", []byte("12345678901234567890")); got != nil {
		t.Errorf("empty password sha2 scramble = %x, want nil", got)
	}
}

func TestScrambleCachingSHA2Length(t *testing.T) {
	got := ScrambleCachingSHA2Password("pw", []byte("12345678901234567890"))
	if len(got) != 32 {
		t.Errorf("sha2 scramble len = %d, want 32", len(got))
	}
}

func TestReadLenencIntErrors(t *testing.T) {
	if _, _, err := ReadLenencInt([]byte{0xFC, 0x01}, 0); err == nil {
		t.Error("expected truncated 2-byte error")
	}
	if _, _, err := ReadLenencInt(nil, 0); err == nil {
		t.Error("expected out-of-range error")
	}
}

func TestParseHandshakeResponse41TooShort(t *testing.T) {
	if _, err := ParseHandshakeResponse41(make([]byte, 10)); err == nil {
		t.Error("expected too-short error")
	}
}

func TestAppendLenencStr(t *testing.T) {
	b := AppendLenencStr(nil, []byte("hi"))
	want := []byte{0x02, 'h', 'i'}
	if !reflect.DeepEqual(b, want) {
		t.Errorf("got %x, want %x", b, want)
	}
}
