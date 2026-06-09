package proxy

import (
	"bytes"
	"reflect"
	"testing"
)

func TestBSONRoundTrip(t *testing.T) {
	doc := BSON{
		{Key: "hello", Value: int32(1)},
		{Key: "ok", Value: 1.0},
		{Key: "msg", Value: "isdbgrid"},
		{Key: "flag", Value: true},
		{Key: "off", Value: false},
		{Key: "null", Value: nil},
		{Key: "big", Value: int64(1) << 40},
		{Key: "when", Value: BSONDateTime(1717545600000)},
		{Key: "ts", Value: BSONTimestamp{T: 1717545600, I: 7}},
		{Key: "id", Value: BSONObjectID{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
		{Key: "payload", Value: BSONBinary{Subtype: BinarySubtypeGeneric, Data: []byte("n,,n=user,r=abc")}},
		{Key: "tags", Value: []any{"a", "b", int32(3)}},
		{Key: "nested", Value: BSON{{Key: "inner", Value: "x"}}},
	}

	encoded := MarshalBSON(doc)
	got, n, err := ReadBSONDocument(encoded)
	if err != nil {
		t.Fatalf("ReadBSONDocument: %v", err)
	}
	if n != len(encoded) {
		t.Fatalf("consumed %d bytes, document is %d", n, len(encoded))
	}
	if !reflect.DeepEqual(got, doc) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", got, doc)
	}
}

func TestBSONGoldenInt32(t *testing.T) {
	// {"hello": 1} as int32, per the BSON spec, byte-for-byte.
	want := []byte{
		0x10, 0x00, 0x00, 0x00, // total length = 16
		0x10,                          // type int32
		'h', 'e', 'l', 'l', 'o', 0x00, // key "hello"
		0x01, 0x00, 0x00, 0x00, // value 1
		0x00, // terminator
	}
	got := MarshalBSON(BSON{{Key: "hello", Value: int32(1)}})
	if !bytes.Equal(got, want) {
		t.Fatalf("golden mismatch:\n got %x\nwant %x", got, want)
	}
}

func TestBSONIntEncodesAsInt32(t *testing.T) {
	// Go int is encoded as a BSON int32 and decoded back as int32.
	encoded := MarshalBSON(BSON{{Key: "n", Value: 5}})
	got, _, err := ReadBSONDocument(encoded)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := got.Lookup("n")
	if !ok {
		t.Fatal("missing key n")
	}
	if v != int32(5) {
		t.Fatalf("got %#v (%T), want int32(5)", v, v)
	}
}

func TestBSONLookup(t *testing.T) {
	doc := BSON{{Key: "a", Value: int32(1)}, {Key: "b", Value: "two"}}
	if v, ok := doc.Lookup("b"); !ok || v != "two" {
		t.Fatalf("Lookup(b) = %v, %v", v, ok)
	}
	if _, ok := doc.Lookup("missing"); ok {
		t.Fatal("Lookup(missing) should report not found")
	}
}

func TestBSONReadRejectsMalformed(t *testing.T) {
	good := MarshalBSON(BSON{{Key: "x", Value: int32(1)}})

	cases := map[string][]byte{
		"empty":             nil,
		"too short":         {0x05, 0x00, 0x00},
		"length too big":    {0xFF, 0xFF, 0x00, 0x00, 0x00},
		"length below min":  {0x04, 0x00, 0x00, 0x00},
		"truncated value":   good[:len(good)-2],
		"unknown elem type": {0x0c, 0x00, 0x00, 0x00, 0x7f, 'k', 0x00, 0x00, 0x00, 0x00, 0x00, 0x00},
	}
	for name, b := range cases {
		if _, _, err := ReadBSONDocument(b); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestBSONReadReportsConsumedLength(t *testing.T) {
	// A document followed by trailing bytes must report only its own length.
	doc := MarshalBSON(BSON{{Key: "k", Value: "v"}})
	stream := append(append([]byte(nil), doc...), 0xAA, 0xBB)
	_, n, err := ReadBSONDocument(stream)
	if err != nil {
		t.Fatal(err)
	}
	if n != len(doc) {
		t.Fatalf("consumed %d, want %d", n, len(doc))
	}
}
