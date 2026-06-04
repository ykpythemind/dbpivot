package proxy

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
)

func TestMongoMessageRoundTrip(t *testing.T) {
	body := EncodeOpMsgBody(0, BSON{{Key: "ping", Value: int32(1)}})

	var buf bytes.Buffer
	if err := WriteMongoMessage(&buf, 42, 7, OpMsg, body); err != nil {
		t.Fatalf("WriteMongoMessage: %v", err)
	}

	hdr, gotBody, err := ReadMongoMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMongoMessage: %v", err)
	}
	if hdr.MessageLength != int32(msgHeaderLen+len(body)) {
		t.Errorf("MessageLength = %d, want %d", hdr.MessageLength, msgHeaderLen+len(body))
	}
	if hdr.RequestID != 42 || hdr.ResponseTo != 7 || hdr.OpCode != OpMsg {
		t.Errorf("header = %+v", hdr)
	}
	if !bytes.Equal(gotBody, body) {
		t.Errorf("body mismatch:\n got %x\nwant %x", gotBody, body)
	}
}

func TestReadMessageRejectsBadLength(t *testing.T) {
	mkHeader := func(length int32) []byte {
		b := make([]byte, msgHeaderLen)
		binary.LittleEndian.PutUint32(b[0:4], uint32(length))
		binary.LittleEndian.PutUint32(b[12:16], OpMsg)
		return b
	}
	cases := map[string]int32{
		"below header": 4,
		"above max":    MaxMessageSize + 1,
	}
	for name, length := range cases {
		if _, _, err := ReadMongoMessage(bytes.NewReader(mkHeader(length))); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

func TestOpMsgRoundTrip(t *testing.T) {
	doc := BSON{
		{Key: "hello", Value: int32(1)},
		{Key: "$db", Value: "admin"},
	}
	body := EncodeOpMsgBody(OpMsgFlagExhaustAllowed, doc)

	flags, got, err := ParseOpMsg(body)
	if err != nil {
		t.Fatalf("ParseOpMsg: %v", err)
	}
	if flags != OpMsgFlagExhaustAllowed {
		t.Errorf("flags = 0x%x, want 0x%x", flags, OpMsgFlagExhaustAllowed)
	}
	if !reflect.DeepEqual(got, doc) {
		t.Errorf("doc mismatch:\n got %#v\nwant %#v", got, doc)
	}
}

func TestParseOpMsgTrimsChecksum(t *testing.T) {
	doc := BSON{{Key: "ping", Value: int32(1)}}
	body := EncodeOpMsgBody(0, doc)
	// Re-stamp the flags with checksumPresent and append a fake 4-byte CRC.
	binary.LittleEndian.PutUint32(body[0:4], OpMsgFlagChecksumPresent)
	body = append(body, 0xDE, 0xAD, 0xBE, 0xEF)

	flags, got, err := ParseOpMsg(body)
	if err != nil {
		t.Fatalf("ParseOpMsg: %v", err)
	}
	if flags&OpMsgFlagChecksumPresent == 0 {
		t.Error("checksum flag lost")
	}
	if !reflect.DeepEqual(got, doc) {
		t.Errorf("doc mismatch:\n got %#v\nwant %#v", got, doc)
	}
}

func TestParseOpMsgSkipsDocumentSequence(t *testing.T) {
	bodyDoc := BSON{{Key: "insert", Value: "coll"}, {Key: "$db", Value: "app"}}

	// kind-1 document-sequence section: int32 size | cstring ident | docs.
	ident := append([]byte("documents"), 0)
	seqDocs := append(MarshalBSON(BSON{{Key: "_id", Value: int32(1)}}),
		MarshalBSON(BSON{{Key: "_id", Value: int32(2)}})...)
	size := 4 + len(ident) + len(seqDocs)
	section := binary.LittleEndian.AppendUint32(nil, uint32(size))
	section = append(section, ident...)
	section = append(section, seqDocs...)

	body := binary.LittleEndian.AppendUint32(nil, 0)
	body = append(body, 0) // kind-0 body section
	body = AppendBSON(body, bodyDoc)
	body = append(body, 1) // kind-1 document-sequence section
	body = append(body, section...)

	_, got, err := ParseOpMsg(body)
	if err != nil {
		t.Fatalf("ParseOpMsg: %v", err)
	}
	if !reflect.DeepEqual(got, bodyDoc) {
		t.Errorf("body doc mismatch:\n got %#v\nwant %#v", got, bodyDoc)
	}
}

func TestParseOpMsgErrors(t *testing.T) {
	if _, _, err := ParseOpMsg([]byte{0x00, 0x00}); err == nil {
		t.Error("short body: expected error")
	}
	// flags only, no sections -> no body section.
	if _, _, err := ParseOpMsg([]byte{0, 0, 0, 0}); err == nil {
		t.Error("no body section: expected error")
	}
	// unknown section kind.
	bad := append(binary.LittleEndian.AppendUint32(nil, 0), 0x09)
	if _, _, err := ParseOpMsg(bad); err == nil {
		t.Error("unknown section kind: expected error")
	}
}

func TestOpQueryParse(t *testing.T) {
	query := BSON{{Key: "isMaster", Value: int32(1)}}
	body := binary.LittleEndian.AppendUint32(nil, 0) // flags
	body = append(body, "admin.$cmd"...)
	body = append(body, 0)
	body = binary.LittleEndian.AppendUint32(body, 0)          // numberToSkip
	body = binary.LittleEndian.AppendUint32(body, ^uint32(0)) // numberToReturn = -1
	body = AppendBSON(body, query)

	flags, coll, skip, ret, gotQuery, err := ParseOpQuery(body)
	if err != nil {
		t.Fatalf("ParseOpQuery: %v", err)
	}
	if flags != 0 || coll != "admin.$cmd" || skip != 0 || ret != -1 {
		t.Errorf("flags=%d coll=%q skip=%d ret=%d", flags, coll, skip, ret)
	}
	if !reflect.DeepEqual(gotQuery, query) {
		t.Errorf("query mismatch:\n got %#v\nwant %#v", gotQuery, query)
	}
}

func TestOpReplyEncode(t *testing.T) {
	docs := []BSON{
		{{Key: "ismaster", Value: true}, {Key: "ok", Value: 1.0}},
	}
	body := EncodeOpReplyBody(0, 0, 0, docs)

	// responseFlags(4) + cursorID(8) + startingFrom(4) + numberReturned(4) = 20
	if got := int32(binary.LittleEndian.Uint32(body[16:20])); got != 1 {
		t.Fatalf("numberReturned = %d, want 1", got)
	}
	got, n, err := ReadBSONDocument(body[20:])
	if err != nil {
		t.Fatalf("ReadBSONDocument: %v", err)
	}
	if n != len(body)-20 {
		t.Errorf("consumed %d, want %d", n, len(body)-20)
	}
	if !reflect.DeepEqual(got, docs[0]) {
		t.Errorf("doc mismatch:\n got %#v\nwant %#v", got, docs[0])
	}
}
