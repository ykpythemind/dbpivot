package proxy

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
)

// MySQL wire protocol primitives.
//
// This file implements the framing, integer/string encodings, handshake
// packets, and authentication scrambles needed to proxy the MySQL protocol.
// It deliberately mirrors the shape of pgwire.go: pure functions over byte
// slices plus a small set of net.Conn-free read/write helpers so they can be
// unit-tested in isolation, then wired into the server later.
//
// References: MySQL Client/Server Protocol (protocol version 10), used by
// MySQL 5.x/8.x and MariaDB.

// MaxPacketPayload is the largest single-packet payload. Payloads at or above
// this size are split across multiple packets by the protocol; the handshake
// phase never reaches it, so the helpers here treat it as a hard cap.
const MaxPacketPayload = 0xFFFFFF // 16 MiB - 1

// MySQL capability flags (subset relevant to the proxy).
const (
	ClientLongPassword               = 0x00000001
	ClientFoundRows                  = 0x00000002
	ClientLongFlag                   = 0x00000004
	ClientConnectWithDB              = 0x00000008
	ClientProtocol41                 = 0x00000200
	ClientSSL                        = 0x00000800
	ClientTransactions               = 0x00002000
	ClientSecureConnection           = 0x00008000
	ClientPluginAuth                 = 0x00080000
	ClientConnectAttrs               = 0x00100000
	ClientPluginAuthLenencClientData = 0x00200000
	ClientDeprecateEOF               = 0x01000000
)

// MySQL packet header bytes for OK / ERR / EOF generic responses.
const (
	OKPacketHeader  = 0x00
	ErrPacketHeader = 0xFF
	EOFPacketHeader = 0xFE
)

// Auth plugin names.
const (
	MySQLNativePassword   = "mysql_native_password"
	CachingSHA2Password   = "caching_sha2_password"
	ProtocolVersion10     = 0x0A
	DefaultCharsetUTF8MB4 = 0x2D // utf8mb4_general_ci (45)
)

// ReadPacket reads a single MySQL packet: a 3-byte little-endian payload
// length, a 1-byte sequence id, then the payload. Multi-packet payloads (a
// full 0xFFFFFF-byte packet followed by a continuation) are reassembled.
func ReadPacket(r io.Reader) (seq byte, payload []byte, err error) {
	var hdr [4]byte
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			return 0, nil, err
		}
		length := int(hdr[0]) | int(hdr[1])<<8 | int(hdr[2])<<16
		seq = hdr[3]
		chunk := make([]byte, length)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return 0, nil, err
		}
		payload = append(payload, chunk...)
		if length < MaxPacketPayload {
			return seq, payload, nil
		}
		// A maximal packet signals a continuation packet follows.
	}
}

// WritePacket writes payload as one or more MySQL packets, incrementing the
// sequence id for each frame. It returns the next sequence id to use.
func WritePacket(w io.Writer, seq byte, payload []byte) (nextSeq byte, err error) {
	for {
		n := len(payload)
		if n > MaxPacketPayload {
			n = MaxPacketPayload
		}
		var hdr [4]byte
		hdr[0] = byte(n)
		hdr[1] = byte(n >> 8)
		hdr[2] = byte(n >> 16)
		hdr[3] = seq
		if _, err := w.Write(hdr[:]); err != nil {
			return seq, err
		}
		if _, err := w.Write(payload[:n]); err != nil {
			return seq, err
		}
		seq++
		payload = payload[n:]
		// A payload that is an exact multiple of MaxPacketPayload requires a
		// trailing empty packet so the receiver knows it ended.
		if n < MaxPacketPayload {
			return seq, nil
		}
		if len(payload) == 0 {
			// emit terminating zero-length packet
			var z [4]byte
			z[3] = seq
			if _, err := w.Write(z[:]); err != nil {
				return seq, err
			}
			seq++
			return seq, nil
		}
	}
}

// --- length-encoded integers / strings ---

// AppendLenencInt appends i to b using MySQL's length-encoded integer format.
func AppendLenencInt(b []byte, i uint64) []byte {
	switch {
	case i < 0xFB:
		return append(b, byte(i))
	case i <= 0xFFFF:
		return append(b, 0xFC, byte(i), byte(i>>8))
	case i <= 0xFFFFFF:
		return append(b, 0xFD, byte(i), byte(i>>8), byte(i>>16))
	default:
		return append(b, 0xFE,
			byte(i), byte(i>>8), byte(i>>16), byte(i>>24),
			byte(i>>32), byte(i>>40), byte(i>>48), byte(i>>56))
	}
}

// ReadLenencInt decodes a length-encoded integer at b[pos], returning the
// value and the position just past it.
func ReadLenencInt(b []byte, pos int) (val uint64, next int, err error) {
	if pos >= len(b) {
		return 0, pos, fmt.Errorf("lenenc-int: out of range at %d", pos)
	}
	first := b[pos]
	switch {
	case first < 0xFB:
		return uint64(first), pos + 1, nil
	case first == 0xFC:
		if pos+3 > len(b) {
			return 0, pos, fmt.Errorf("lenenc-int: truncated 2-byte")
		}
		return uint64(b[pos+1]) | uint64(b[pos+2])<<8, pos + 3, nil
	case first == 0xFD:
		if pos+4 > len(b) {
			return 0, pos, fmt.Errorf("lenenc-int: truncated 3-byte")
		}
		return uint64(b[pos+1]) | uint64(b[pos+2])<<8 | uint64(b[pos+3])<<16, pos + 4, nil
	case first == 0xFE:
		if pos+9 > len(b) {
			return 0, pos, fmt.Errorf("lenenc-int: truncated 8-byte")
		}
		return binary.LittleEndian.Uint64(b[pos+1 : pos+9]), pos + 9, nil
	default:
		// 0xFB (NULL) and 0xFF are not valid here.
		return 0, pos, fmt.Errorf("lenenc-int: invalid leading byte 0x%02x", first)
	}
}

// AppendLenencStr appends s as a length-encoded string (lenenc length + bytes).
func AppendLenencStr(b []byte, s []byte) []byte {
	b = AppendLenencInt(b, uint64(len(s)))
	return append(b, s...)
}

// readNulString reads a NUL-terminated string at b[pos], returning the string
// and the position just past the NUL.
func readNulString(b []byte, pos int) (string, int, error) {
	for i := pos; i < len(b); i++ {
		if b[i] == 0 {
			return string(b[pos:i]), i + 1, nil
		}
	}
	return "", pos, fmt.Errorf("nul-string: unterminated at %d", pos)
}

// --- handshake v10 (server -> client) ---

// HandshakeV10 is the parsed initial handshake the upstream server sends.
type HandshakeV10 struct {
	ProtocolVersion byte
	ServerVersion   string
	ConnectionID    uint32
	AuthPluginData  []byte // concatenated salt (part1 + part2)
	Capabilities    uint32
	CharacterSet    byte
	StatusFlags     uint16
	AuthPluginName  string
}

// ParseHandshakeV10 decodes a protocol-version-10 initial handshake payload.
func ParseHandshakeV10(b []byte) (*HandshakeV10, error) {
	if len(b) < 1 {
		return nil, fmt.Errorf("handshake: empty")
	}
	h := &HandshakeV10{ProtocolVersion: b[0]}
	if h.ProtocolVersion != ProtocolVersion10 {
		// 0xFF here would be an ERR packet (e.g. "too many connections").
		if h.ProtocolVersion == ErrPacketHeader {
			return nil, fmt.Errorf("server returned error instead of handshake: %s", ParseERRPacket(b))
		}
		return nil, fmt.Errorf("unsupported handshake protocol version %d", h.ProtocolVersion)
	}
	pos := 1
	var err error
	h.ServerVersion, pos, err = readNulString(b, pos)
	if err != nil {
		return nil, fmt.Errorf("handshake server version: %w", err)
	}
	if pos+4 > len(b) {
		return nil, fmt.Errorf("handshake: truncated connection id")
	}
	h.ConnectionID = binary.LittleEndian.Uint32(b[pos : pos+4])
	pos += 4
	if pos+8 > len(b) {
		return nil, fmt.Errorf("handshake: truncated auth-plugin-data-part-1")
	}
	authData := make([]byte, 8)
	copy(authData, b[pos:pos+8])
	pos += 8
	// filler
	if pos >= len(b) {
		return nil, fmt.Errorf("handshake: truncated filler")
	}
	pos++ // skip filler 0x00
	if pos+2 > len(b) {
		return nil, fmt.Errorf("handshake: truncated capability lower")
	}
	capLower := binary.LittleEndian.Uint16(b[pos : pos+2])
	pos += 2
	h.Capabilities = uint32(capLower)
	// Older/shorter handshakes may stop here.
	if pos < len(b) {
		h.CharacterSet = b[pos]
		pos++
	}
	if pos+2 <= len(b) {
		h.StatusFlags = binary.LittleEndian.Uint16(b[pos : pos+2])
		pos += 2
	}
	if pos+2 <= len(b) {
		capUpper := binary.LittleEndian.Uint16(b[pos : pos+2])
		pos += 2
		h.Capabilities |= uint32(capUpper) << 16
	}
	var authDataLen byte
	if pos < len(b) {
		authDataLen = b[pos]
		pos++
	}
	// reserved 10 bytes
	if pos+10 <= len(b) {
		pos += 10
	}
	if h.Capabilities&ClientSecureConnection != 0 {
		// part 2: at least 13 bytes, total auth data is authDataLen.
		part2Len := 13
		if int(authDataLen)-8 > part2Len {
			part2Len = int(authDataLen) - 8
		}
		if pos+part2Len > len(b) {
			part2Len = len(b) - pos
		}
		if part2Len > 0 {
			part2 := b[pos : pos+part2Len]
			// drop the trailing NUL the server appends.
			part2 = bytes.TrimRight(part2, "\x00")
			authData = append(authData, part2...)
			pos += part2Len
		}
	}
	h.AuthPluginData = authData
	if h.Capabilities&ClientPluginAuth != 0 && pos < len(b) {
		name, _, err := readNulString(b, pos)
		if err != nil {
			// Some servers omit the trailing NUL; take the rest.
			name = string(bytes.TrimRight(b[pos:], "\x00"))
		}
		h.AuthPluginName = name
	}
	return h, nil
}

// EncodeHandshakeV10 builds an initial handshake payload (server -> client),
// used when the proxy greets a client directly. salt must be 20 bytes; it is
// split into the protocol's 8-byte part-1 and 12-byte (+NUL) part-2.
func EncodeHandshakeV10(serverVersion string, connectionID uint32, capabilities uint32, charset byte, statusFlags uint16, authPlugin string, salt []byte) []byte {
	if len(salt) < 20 {
		padded := make([]byte, 20)
		copy(padded, salt)
		salt = padded
	}
	b := []byte{ProtocolVersion10}
	b = append(b, serverVersion...)
	b = append(b, 0)
	b = binary.LittleEndian.AppendUint32(b, connectionID)
	b = append(b, salt[:8]...) // auth-plugin-data-part-1
	b = append(b, 0)           // filler
	b = binary.LittleEndian.AppendUint16(b, uint16(capabilities))
	b = append(b, charset)
	b = binary.LittleEndian.AppendUint16(b, statusFlags)
	b = binary.LittleEndian.AppendUint16(b, uint16(capabilities>>16))
	if capabilities&ClientPluginAuth != 0 {
		b = append(b, 21) // length of auth-plugin-data (20 + NUL)
	} else {
		b = append(b, 0)
	}
	b = append(b, make([]byte, 10)...) // reserved
	// auth-plugin-data-part-2: 12 bytes + trailing NUL (total 13).
	b = append(b, salt[8:20]...)
	b = append(b, 0)
	if capabilities&ClientPluginAuth != 0 {
		b = append(b, authPlugin...)
		b = append(b, 0)
	}
	return b
}

// EncodeHandshakeResponse41 builds a client login payload (client -> server),
// used by the proxy when authenticating to the upstream. The auth-response is
// always written length-encoded as a single byte length (CLIENT_SECURE_CONNECTION
// form), which every modern server accepts.
func EncodeHandshakeResponse41(capabilities uint32, charset byte, username string, authResponse []byte, database, authPlugin string) []byte {
	b := binary.LittleEndian.AppendUint32(nil, capabilities)
	b = binary.LittleEndian.AppendUint32(b, MaxPacketPayload)
	b = append(b, charset)
	b = append(b, make([]byte, 23)...) // reserved
	b = append(b, username...)
	b = append(b, 0)
	b = append(b, byte(len(authResponse)))
	b = append(b, authResponse...)
	if capabilities&ClientConnectWithDB != 0 {
		b = append(b, database...)
		b = append(b, 0)
	}
	if capabilities&ClientPluginAuth != 0 {
		b = append(b, authPlugin...)
		b = append(b, 0)
	}
	return b
}

// EncodeSSLRequest builds the short SSLRequest packet a client sends before a
// TLS upgrade: it is exactly the 32-byte fixed header of HandshakeResponse41
// (capabilities — which must include CLIENT_SSL — max packet size, charset, and
// 23 reserved bytes) with no username/auth payload. After the server reads this
// and the TLS handshake completes, the full HandshakeResponse41 follows
// encrypted, continuing the same sequence id counter.
func EncodeSSLRequest(capabilities uint32, charset byte) []byte {
	b := binary.LittleEndian.AppendUint32(nil, capabilities)
	b = binary.LittleEndian.AppendUint32(b, MaxPacketPayload)
	b = append(b, charset)
	b = append(b, make([]byte, 23)...) // reserved
	return b
}

// --- handshake response 41 (client -> server) ---

// HandshakeResponse41 is the parsed client login packet.
type HandshakeResponse41 struct {
	Capabilities  uint32
	MaxPacketSize uint32
	CharacterSet  byte
	Username      string
	AuthResponse  []byte
	Database      string
	AuthPlugin    string
}

// ParseHandshakeResponse41 decodes a client HandshakeResponse41 payload.
func ParseHandshakeResponse41(b []byte) (*HandshakeResponse41, error) {
	if len(b) < 32 {
		return nil, fmt.Errorf("handshake response: too short (%d)", len(b))
	}
	r := &HandshakeResponse41{}
	r.Capabilities = binary.LittleEndian.Uint32(b[0:4])
	if r.Capabilities&ClientProtocol41 == 0 {
		return nil, fmt.Errorf("handshake response: pre-4.1 protocol not supported")
	}
	r.MaxPacketSize = binary.LittleEndian.Uint32(b[4:8])
	r.CharacterSet = b[8]
	// 23 bytes reserved
	pos := 32
	var err error
	r.Username, pos, err = readNulString(b, pos)
	if err != nil {
		return nil, fmt.Errorf("handshake response username: %w", err)
	}
	if r.Capabilities&ClientPluginAuthLenencClientData != 0 {
		var n uint64
		n, pos, err = ReadLenencInt(b, pos)
		if err != nil {
			return nil, fmt.Errorf("handshake response auth len: %w", err)
		}
		if pos+int(n) > len(b) {
			return nil, fmt.Errorf("handshake response: auth-response out of range")
		}
		r.AuthResponse = append([]byte(nil), b[pos:pos+int(n)]...)
		pos += int(n)
	} else if r.Capabilities&ClientSecureConnection != 0 {
		if pos >= len(b) {
			return nil, fmt.Errorf("handshake response: missing auth length")
		}
		n := int(b[pos])
		pos++
		if pos+n > len(b) {
			return nil, fmt.Errorf("handshake response: auth-response out of range")
		}
		r.AuthResponse = append([]byte(nil), b[pos:pos+n]...)
		pos += n
	} else {
		var s string
		s, pos, err = readNulString(b, pos)
		if err != nil {
			return nil, fmt.Errorf("handshake response auth: %w", err)
		}
		r.AuthResponse = []byte(s)
	}
	if r.Capabilities&ClientConnectWithDB != 0 {
		r.Database, pos, err = readNulString(b, pos)
		if err != nil {
			return nil, fmt.Errorf("handshake response database: %w", err)
		}
	}
	if r.Capabilities&ClientPluginAuth != 0 {
		// Auth plugin name may be absent on some clients; tolerate.
		if pos < len(b) {
			name, _, perr := readNulString(b, pos)
			if perr == nil {
				r.AuthPlugin = name
			}
		}
	}
	return r, nil
}

// --- OK / ERR packet builders & parsers ---

// EncodeOKPacket builds an OK packet body for a 4.1+ client.
func EncodeOKPacket(affectedRows, lastInsertID uint64, statusFlags, warnings uint16) []byte {
	b := []byte{OKPacketHeader}
	b = AppendLenencInt(b, affectedRows)
	b = AppendLenencInt(b, lastInsertID)
	b = binary.LittleEndian.AppendUint16(b, statusFlags)
	b = binary.LittleEndian.AppendUint16(b, warnings)
	return b
}

// EncodeERRPacket builds an ERR packet body for a 4.1+ client (with SQL state).
func EncodeERRPacket(code uint16, sqlState, message string) []byte {
	if len(sqlState) != 5 {
		sqlState = "HY000"
	}
	b := []byte{ErrPacketHeader}
	b = binary.LittleEndian.AppendUint16(b, code)
	b = append(b, '#')
	b = append(b, sqlState...)
	b = append(b, message...)
	return b
}

// ParseERRPacket decodes an ERR packet payload into a human-readable string.
// It tolerates both the 4.1 (with #SQLSTATE) and older formats.
func ParseERRPacket(b []byte) string {
	if len(b) < 3 || b[0] != ErrPacketHeader {
		return "not an ERR packet"
	}
	code := binary.LittleEndian.Uint16(b[1:3])
	pos := 3
	sqlState := ""
	if pos < len(b) && b[pos] == '#' {
		if pos+6 <= len(b) {
			sqlState = string(b[pos+1 : pos+6])
			pos += 6
		}
	}
	msg := string(b[pos:])
	if sqlState != "" {
		return fmt.Sprintf("ERROR %d (%s): %s", code, sqlState, msg)
	}
	return fmt.Sprintf("ERROR %d: %s", code, msg)
}

// IsErrPacket reports whether a payload is an ERR packet.
func IsErrPacket(b []byte) bool { return len(b) > 0 && b[0] == ErrPacketHeader }

// IsOKPacket reports whether a payload is an OK packet. (An EOF-marker 0xFE
// with a short length is not an OK packet; callers in the handshake phase only
// see OK or ERR, so the simple header check suffices there.)
func IsOKPacket(b []byte) bool { return len(b) > 0 && b[0] == OKPacketHeader }

// --- authentication scrambles ---

// ScrambleNativePassword computes the mysql_native_password auth response:
//
//	SHA1(password) XOR SHA1(salt + SHA1(SHA1(password)))
//
// salt is the 20-byte auth-plugin-data from the server handshake. An empty
// password yields an empty response (matching the protocol).
func ScrambleNativePassword(password string, salt []byte) []byte {
	if password == "" {
		return nil
	}
	if len(salt) > 20 {
		salt = salt[:20]
	}
	h1 := sha1Sum([]byte(password))
	h2 := sha1Sum(h1)
	h3 := sha1Sum(append(append([]byte(nil), salt...), h2...))
	out := make([]byte, len(h1))
	for i := range h1 {
		out[i] = h1[i] ^ h3[i]
	}
	return out
}

// ScrambleCachingSHA2Password computes the caching_sha2_password fast-auth
// response:
//
//	SHA256(password) XOR SHA256(SHA256(SHA256(password)) + salt)
func ScrambleCachingSHA2Password(password string, salt []byte) []byte {
	if password == "" {
		return nil
	}
	if len(salt) > 20 {
		salt = salt[:20]
	}
	h1 := sha256Sum([]byte(password))
	h2 := sha256Sum(h1)
	h3 := sha256Sum(append(append([]byte(nil), h2...), salt...))
	out := make([]byte, len(h1))
	for i := range h1 {
		out[i] = h1[i] ^ h3[i]
	}
	return out
}

func sha1Sum(b []byte) []byte {
	s := sha1.Sum(b)
	return s[:]
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}
