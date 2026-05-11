package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
)

// PostgreSQL wire protocol special codes (read from the message body before
// any regular framing — they sit where the protocol version normally would).
const (
	SSLRequestCode    = 0x04D2162F // 80877103
	GSSENCRequestCode = 0x04D21630 // 80877104
	CancelRequestCode = 0x04D2162E // 80877102
	ProtocolV3        = 0x00030000 // 196608
)

const MaxStartupLen = 64 * 1024

// PostgreSQL Authentication subcodes (inside a message of type 'R').
const (
	AuthOk                = 0
	AuthCleartextPassword = 3
	AuthMD5Password       = 5
	AuthSASL              = 10
	AuthSASLContinue      = 11
	AuthSASLFinal         = 12
)

// KV is an ordered (key, value) pair from a StartupMessage.
type KV struct {
	K, V string
}

// ParseStartupBody decodes the parameter block of a StartupMessage: a series
// of null-terminated key/value pairs terminated by a single null byte.
// Order is preserved.
func ParseStartupBody(b []byte) ([]KV, error) {
	if len(b) == 0 || b[len(b)-1] != 0 {
		return nil, fmt.Errorf("startup body: missing trailing NUL")
	}
	var out []KV
	cursor := 0
	for cursor < len(b)-1 {
		keyEnd := indexByte(b, cursor, 0)
		if keyEnd < 0 {
			return nil, fmt.Errorf("startup body: unterminated key at %d", cursor)
		}
		key := string(b[cursor:keyEnd])
		if key == "" {
			return nil, fmt.Errorf("startup body: empty key at %d", cursor)
		}
		valStart := keyEnd + 1
		valEnd := indexByte(b, valStart, 0)
		if valEnd < 0 {
			return nil, fmt.Errorf("startup body: unterminated value for key %q", key)
		}
		val := string(b[valStart:valEnd])
		out = append(out, KV{K: key, V: val})
		cursor = valEnd + 1
	}
	if cursor != len(b)-1 {
		return nil, fmt.Errorf("startup body: missing end-of-params NUL")
	}
	return out, nil
}

func indexByte(b []byte, from int, c byte) int {
	for i := from; i < len(b); i++ {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// LookupParam returns the value of the first KV with the given key, or "".
func LookupParam(p []KV, key string) string {
	for _, kv := range p {
		if kv.K == key {
			return kv.V
		}
	}
	return ""
}

// UpsertParam replaces the first KV with the given key, preserving order.
// If no such KV exists, appends one at the end.
func UpsertParam(p []KV, key, value string) []KV {
	for i := range p {
		if p[i].K == key {
			p[i].V = value
			return p
		}
	}
	return append(p, KV{K: key, V: value})
}

// EncodeStartup builds a full StartupMessage frame:
// [int32 length | int32 protocol | (key\0value\0)* \0].
func EncodeStartup(params []KV) []byte {
	bodyLen := 1 // trailing NUL
	for _, kv := range params {
		bodyLen += len(kv.K) + 1 + len(kv.V) + 1
	}
	total := 4 + 4 + bodyLen
	out := make([]byte, total)
	binary.BigEndian.PutUint32(out[0:4], uint32(total))
	binary.BigEndian.PutUint32(out[4:8], ProtocolV3)
	pos := 8
	for _, kv := range params {
		copy(out[pos:], kv.K)
		pos += len(kv.K)
		out[pos] = 0
		pos++
		copy(out[pos:], kv.V)
		pos += len(kv.V)
		out[pos] = 0
		pos++
	}
	out[pos] = 0
	return out
}

// ReadMessage reads a single PostgreSQL backend/frontend message
// (1-byte type + 4-byte length + body).
func ReadMessage(r io.Reader) (typ byte, body []byte, err error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	typ = hdr[0]
	length := binary.BigEndian.Uint32(hdr[1:5])
	if length < 4 {
		return 0, nil, fmt.Errorf("message length %d < 4", length)
	}
	bodyLen := int(length) - 4
	if bodyLen < 0 || bodyLen > MaxStartupLen {
		return 0, nil, fmt.Errorf("message body length %d out of range", bodyLen)
	}
	body = make([]byte, bodyLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, nil, err
	}
	return typ, body, nil
}

// WriteMessage writes a typed message: type byte, 4-byte length covering the
// length itself plus body, then the body.
func WriteMessage(w io.Writer, typ byte, body []byte) error {
	hdr := [5]byte{typ}
	binary.BigEndian.PutUint32(hdr[1:5], uint32(4+len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// WriteAuthenticationOk emits 'R' length=8 code=0.
func WriteAuthenticationOk(w io.Writer) error {
	var body [4]byte
	binary.BigEndian.PutUint32(body[:], AuthOk)
	return WriteMessage(w, 'R', body[:])
}

// WriteSASLInitialResponse sends a PasswordMessage ('p') carrying:
//
//	mechanism\0 int32(len(data)) data
func WriteSASLInitialResponse(w io.Writer, mechanism, data string) error {
	body := make([]byte, 0, len(mechanism)+1+4+len(data))
	body = append(body, mechanism...)
	body = append(body, 0)
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(data)))
	body = append(body, l[:]...)
	body = append(body, data...)
	return WriteMessage(w, 'p', body)
}

// WriteSASLResponse sends a PasswordMessage ('p') carrying the SCRAM
// client-final-message bytes verbatim.
func WriteSASLResponse(w io.Writer, data string) error {
	return WriteMessage(w, 'p', []byte(data))
}

// ParseAuthenticationMessage splits an authentication ('R') message body into
// the auth subcode and the trailing data (if any).
func ParseAuthenticationMessage(body []byte) (code int32, data []byte, err error) {
	if len(body) < 4 {
		return 0, nil, fmt.Errorf("authentication message too short: %d", len(body))
	}
	code = int32(binary.BigEndian.Uint32(body[:4]))
	return code, body[4:], nil
}

// WriteErrorResponse emits an ErrorResponse ('E') with minimum useful fields.
// The connection should be closed after this call; clients treat it as
// terminal during the auth phase.
func WriteErrorResponse(w io.Writer, severity, sqlstate, message string) error {
	var body []byte
	body = append(body, 'S')
	body = append(body, severity...)
	body = append(body, 0)
	body = append(body, 'V')
	body = append(body, severity...)
	body = append(body, 0)
	body = append(body, 'C')
	body = append(body, sqlstate...)
	body = append(body, 0)
	body = append(body, 'M')
	body = append(body, message...)
	body = append(body, 0)
	body = append(body, 0)
	return WriteMessage(w, 'E', body)
}

// ReadUint32 reads a single big-endian 4-byte unsigned int.
func ReadUint32(r io.Reader) (uint32, error) {
	var b [4]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint32(b[:]), nil
}
