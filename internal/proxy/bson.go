package proxy

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
)

// Minimal BSON codec.
//
// MongoDB speaks BSON, not a flat key/value parameter block like PostgreSQL or
// length-encoded fields like MySQL, so the proxy needs to read and build BSON
// documents to drive the handshake (`hello`) and SCRAM auth (`saslStart` /
// `saslContinue`) exchanges. This file implements just the subset of BSON the
// handshake/auth phase uses: the element types that appear in those command
// documents and their replies. It is deliberately self-contained (no
// dependency on the official mongo-driver) and pure — byte slices in, byte
// slices / Go values out — so it can be unit-tested in isolation and wired
// into the server later, mirroring the shape of pgwire.go / mysqlwire.go.
//
// Reference: the BSON specification (https://bsonspec.org/spec.html).

// BSON element type bytes (subset relevant to the handshake/auth phase).
const (
	bsonDouble    = 0x01
	bsonString    = 0x02
	bsonDocument  = 0x03
	bsonArray     = 0x04
	bsonBinary    = 0x05
	bsonObjectID  = 0x07
	bsonBool      = 0x08
	bsonDateTime  = 0x09
	bsonNull      = 0x0A
	bsonInt32     = 0x10
	bsonTimestamp = 0x11
	bsonInt64     = 0x12
)

// BinarySubtypeGeneric is the default BSON binary subtype (0x00). SASL auth
// payloads are carried as generic binary.
const BinarySubtypeGeneric = 0x00

// BSON is an ordered BSON document. Order is significant in MongoDB commands —
// the command name must be the first element — so a document is always an
// ordered slice, never a Go map.
type BSON []BSONElem

// BSONElem is a single document element (key/value pair). Value holds one of
// the Go types this codec understands: float64, string, BSON (sub-document),
// []any (array), BSONBinary, BSONObjectID, bool, BSONDateTime, nil, int32, int
// (encoded as int32), BSONTimestamp, or int64.
type BSONElem struct {
	Key   string
	Value any
}

// BSONBinary is a BSON binary value: a subtype byte plus raw bytes. The SASL
// conversation payloads exchanged during MongoDB auth are generic binary.
type BSONBinary struct {
	Subtype byte
	Data    []byte
}

// BSONObjectID is a 12-byte BSON ObjectId.
type BSONObjectID [12]byte

// BSONTimestamp is the BSON internal timestamp type (0x11): a seconds value
// plus an ordinal increment within that second.
type BSONTimestamp struct {
	T uint32 // seconds since the Unix epoch
	I uint32 // ordinal increment within the second
}

// BSONDateTime is a BSON UTC datetime (0x09): milliseconds since the Unix epoch.
type BSONDateTime int64

// Lookup returns the value for the first element with the given key.
func (d BSON) Lookup(key string) (any, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

// MarshalBSON encodes d into a freshly allocated BSON document.
func MarshalBSON(d BSON) []byte {
	return AppendBSON(nil, d)
}

// AppendBSON appends the BSON encoding of d to b and returns the extended
// slice. It panics if a value uses a Go type the codec does not support — the
// proxy only ever encodes documents it constructs itself, so an unsupported
// type is a programming error, not bad input.
func AppendBSON(b []byte, d BSON) []byte {
	start := len(b)
	b = append(b, 0, 0, 0, 0) // total length placeholder, filled in below
	for _, e := range d {
		b = appendBSONElement(b, e.Key, e.Value)
	}
	b = append(b, 0) // document terminator
	binary.LittleEndian.PutUint32(b[start:], uint32(len(b)-start))
	return b
}

func appendBSONElement(b []byte, key string, v any) []byte {
	switch val := v.(type) {
	case float64:
		b = append(b, bsonDouble)
		b = appendCString(b, key)
		b = binary.LittleEndian.AppendUint64(b, math.Float64bits(val))
	case string:
		b = append(b, bsonString)
		b = appendCString(b, key)
		b = appendBSONString(b, val)
	case BSON:
		b = append(b, bsonDocument)
		b = appendCString(b, key)
		b = AppendBSON(b, val)
	case []any:
		b = append(b, bsonArray)
		b = appendCString(b, key)
		arr := make(BSON, len(val))
		for i, e := range val {
			arr[i] = BSONElem{Key: strconv.Itoa(i), Value: e}
		}
		b = AppendBSON(b, arr)
	case BSONBinary:
		b = append(b, bsonBinary)
		b = appendCString(b, key)
		b = binary.LittleEndian.AppendUint32(b, uint32(len(val.Data)))
		b = append(b, val.Subtype)
		b = append(b, val.Data...)
	case BSONObjectID:
		b = append(b, bsonObjectID)
		b = appendCString(b, key)
		b = append(b, val[:]...)
	case bool:
		b = append(b, bsonBool)
		b = appendCString(b, key)
		if val {
			b = append(b, 1)
		} else {
			b = append(b, 0)
		}
	case BSONDateTime:
		b = append(b, bsonDateTime)
		b = appendCString(b, key)
		b = binary.LittleEndian.AppendUint64(b, uint64(int64(val)))
	case nil:
		b = append(b, bsonNull)
		b = appendCString(b, key)
	case int32:
		b = append(b, bsonInt32)
		b = appendCString(b, key)
		b = binary.LittleEndian.AppendUint32(b, uint32(val))
	case int:
		b = append(b, bsonInt32)
		b = appendCString(b, key)
		b = binary.LittleEndian.AppendUint32(b, uint32(int32(val)))
	case BSONTimestamp:
		b = append(b, bsonTimestamp)
		b = appendCString(b, key)
		b = binary.LittleEndian.AppendUint32(b, val.I)
		b = binary.LittleEndian.AppendUint32(b, val.T)
	case int64:
		b = append(b, bsonInt64)
		b = appendCString(b, key)
		b = binary.LittleEndian.AppendUint64(b, uint64(val))
	default:
		panic(fmt.Sprintf("bson: unsupported Go type %T for key %q", v, key))
	}
	return b
}

func appendCString(b []byte, s string) []byte {
	b = append(b, s...)
	return append(b, 0)
}

func appendBSONString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)+1))
	b = append(b, s...)
	return append(b, 0)
}

// ReadBSONDocument decodes the BSON document at the start of b, returning the
// parsed document and the number of bytes it occupied (so callers parsing a
// stream of documents can advance). It rejects truncated, over-long, or
// malformed documents rather than reading past the declared length.
func ReadBSONDocument(b []byte) (BSON, int, error) {
	if len(b) < 5 {
		return nil, 0, fmt.Errorf("bson: document too short (%d bytes)", len(b))
	}
	length := int(int32(binary.LittleEndian.Uint32(b[0:4])))
	if length < 5 || length > len(b) {
		return nil, 0, fmt.Errorf("bson: invalid document length %d (have %d bytes)", length, len(b))
	}
	doc := b[:length]
	pos := 4
	var out BSON
	for {
		if pos >= length {
			return nil, 0, fmt.Errorf("bson: document not terminated")
		}
		t := doc[pos]
		pos++
		if t == 0x00 {
			if pos != length {
				return nil, 0, fmt.Errorf("bson: %d trailing bytes after terminator", length-pos)
			}
			return out, length, nil
		}
		key, np, err := readCString(doc, pos)
		if err != nil {
			return nil, 0, err
		}
		pos = np
		val, np, err := readBSONValue(t, doc, pos)
		if err != nil {
			return nil, 0, err
		}
		pos = np
		out = append(out, BSONElem{Key: key, Value: val})
	}
}

func readBSONValue(t byte, b []byte, pos int) (any, int, error) {
	switch t {
	case bsonDouble:
		if pos+8 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated double")
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b[pos : pos+8])), pos + 8, nil
	case bsonString:
		return readBSONString(b, pos)
	case bsonDocument:
		sub, n, err := ReadBSONDocument(b[pos:])
		if err != nil {
			return nil, 0, err
		}
		return sub, pos + n, nil
	case bsonArray:
		sub, n, err := ReadBSONDocument(b[pos:])
		if err != nil {
			return nil, 0, err
		}
		arr := make([]any, len(sub))
		for i, e := range sub {
			arr[i] = e.Value
		}
		return arr, pos + n, nil
	case bsonBinary:
		if pos+5 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated binary header")
		}
		blen := int(int32(binary.LittleEndian.Uint32(b[pos : pos+4])))
		subtype := b[pos+4]
		start := pos + 5
		if blen < 0 || start+blen > len(b) {
			return nil, 0, fmt.Errorf("bson: binary length %d out of range", blen)
		}
		data := append([]byte(nil), b[start:start+blen]...)
		return BSONBinary{Subtype: subtype, Data: data}, start + blen, nil
	case bsonObjectID:
		if pos+12 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated objectid")
		}
		var oid BSONObjectID
		copy(oid[:], b[pos:pos+12])
		return oid, pos + 12, nil
	case bsonBool:
		if pos+1 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated bool")
		}
		return b[pos] != 0, pos + 1, nil
	case bsonDateTime:
		if pos+8 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated datetime")
		}
		return BSONDateTime(int64(binary.LittleEndian.Uint64(b[pos : pos+8]))), pos + 8, nil
	case bsonNull:
		return nil, pos, nil
	case bsonInt32:
		if pos+4 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated int32")
		}
		return int32(binary.LittleEndian.Uint32(b[pos : pos+4])), pos + 4, nil
	case bsonTimestamp:
		if pos+8 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated timestamp")
		}
		inc := binary.LittleEndian.Uint32(b[pos : pos+4])
		secs := binary.LittleEndian.Uint32(b[pos+4 : pos+8])
		return BSONTimestamp{T: secs, I: inc}, pos + 8, nil
	case bsonInt64:
		if pos+8 > len(b) {
			return nil, 0, fmt.Errorf("bson: truncated int64")
		}
		return int64(binary.LittleEndian.Uint64(b[pos : pos+8])), pos + 8, nil
	default:
		return nil, 0, fmt.Errorf("bson: unsupported element type 0x%02x", t)
	}
}

// readBSONString reads a BSON UTF-8 string: an int32 length (which includes
// the trailing NUL) followed by the bytes and the NUL.
func readBSONString(b []byte, pos int) (string, int, error) {
	if pos+4 > len(b) {
		return "", 0, fmt.Errorf("bson: truncated string length")
	}
	n := int(int32(binary.LittleEndian.Uint32(b[pos : pos+4])))
	if n < 1 {
		return "", 0, fmt.Errorf("bson: invalid string length %d", n)
	}
	start := pos + 4
	if start+n > len(b) {
		return "", 0, fmt.Errorf("bson: string length %d out of range", n)
	}
	if b[start+n-1] != 0 {
		return "", 0, fmt.Errorf("bson: string not NUL-terminated")
	}
	return string(b[start : start+n-1]), start + n, nil
}

func readCString(b []byte, pos int) (string, int, error) {
	for i := pos; i < len(b); i++ {
		if b[i] == 0 {
			return string(b[pos:i]), i + 1, nil
		}
	}
	return "", 0, fmt.Errorf("bson: unterminated cstring at %d", pos)
}
