package proxy

import (
	"bytes"
	"encoding/binary"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestIsHelloCommand(t *testing.T) {
	cases := []struct {
		name string
		doc  BSON
		want bool
	}{
		{"hello", BSON{{Key: "hello", Value: int32(1)}}, true},
		{"isMaster", BSON{{Key: "isMaster", Value: int32(1)}}, true},
		{"ismaster legacy", BSON{{Key: "ismaster", Value: int32(1)}}, true},
		{"ping", BSON{{Key: "ping", Value: int32(1)}}, false},
		{"empty", BSON{}, false},
		// The command name must be the FIRST element; a hello key buried later
		// is not a handshake probe.
		{"hello not first", BSON{{Key: "ping", Value: int32(1)}, {Key: "hello", Value: int32(1)}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsHelloCommand(tc.doc); got != tc.want {
				t.Errorf("IsHelloCommand(%v) = %v, want %v", tc.doc, got, tc.want)
			}
		})
	}
}

func TestBuildHelloReply(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	doc := BuildHelloReply(4242, now)

	// ok must be a float64 1.0 (drivers reject ok:1 as int in some paths).
	if v, _ := doc.Lookup("ok"); v != 1.0 {
		t.Errorf("ok = %#v, want 1.0", v)
	}
	if v, _ := doc.Lookup("isWritablePrimary"); v != true {
		t.Errorf("isWritablePrimary = %#v, want true", v)
	}
	if v, _ := doc.Lookup("maxWireVersion"); v != int32(mongoMaxWireVersion) {
		t.Errorf("maxWireVersion = %#v, want %d", v, mongoMaxWireVersion)
	}
	if v, _ := doc.Lookup("minWireVersion"); v != int32(mongoMinWireVersion) {
		t.Errorf("minWireVersion = %#v, want %d", v, mongoMinWireVersion)
	}
	if v, _ := doc.Lookup("connectionId"); v != int32(4242) {
		t.Errorf("connectionId = %#v, want 4242", v)
	}
	if v, _ := doc.Lookup("localTime"); v != BSONDateTime(now.UnixMilli()) {
		t.Errorf("localTime = %#v, want %d", v, now.UnixMilli())
	}

	// The auth-disabled invariant: the reply must NOT advertise any SASL
	// mechanisms, otherwise a credentialed client would attempt SCRAM against
	// the proxy (which it cannot satisfy).
	if _, ok := doc.Lookup("saslSupportedMechs"); ok {
		t.Error("hello reply advertised saslSupportedMechs; client→proxy must be auth-disabled")
	}

	// Round-trips through the BSON codec.
	if _, _, err := ReadBSONDocument(MarshalBSON(doc)); err != nil {
		t.Fatalf("hello reply is not encodable/decodable: %v", err)
	}
}

func TestReadMongoCommand_OpMsg(t *testing.T) {
	doc := BSON{
		{Key: "hello", Value: int32(1)},
		{Key: "$db", Value: "admin"},
	}
	var buf bytes.Buffer
	if err := WriteMongoMessage(&buf, 7, 0, OpMsg, EncodeOpMsgBody(0, doc)); err != nil {
		t.Fatalf("WriteMongoMessage: %v", err)
	}

	cmd, err := ReadMongoCommand(&buf)
	if err != nil {
		t.Fatalf("ReadMongoCommand: %v", err)
	}
	if cmd.OpCode != OpMsg {
		t.Errorf("OpCode = %d, want OpMsg", cmd.OpCode)
	}
	if cmd.DB != "admin" {
		t.Errorf("DB = %q, want admin", cmd.DB)
	}
	if cmd.RequestID != 7 {
		t.Errorf("RequestID = %d, want 7", cmd.RequestID)
	}
	if cmd.CommandName() != "hello" {
		t.Errorf("CommandName = %q, want hello", cmd.CommandName())
	}
	if !IsHelloCommand(cmd.Doc) {
		t.Error("decoded doc not recognized as hello")
	}
}

func TestReadMongoCommand_OpQuery(t *testing.T) {
	query := BSON{{Key: "isMaster", Value: int32(1)}}
	body := binary.LittleEndian.AppendUint32(nil, 0) // flags
	body = append(body, "admin.$cmd"...)
	body = append(body, 0)
	body = binary.LittleEndian.AppendUint32(body, 0)          // numberToSkip
	body = binary.LittleEndian.AppendUint32(body, ^uint32(0)) // numberToReturn = -1
	body = AppendBSON(body, query)

	var buf bytes.Buffer
	if err := WriteMongoMessage(&buf, 11, 0, OpQuery, body); err != nil {
		t.Fatalf("WriteMongoMessage: %v", err)
	}

	cmd, err := ReadMongoCommand(&buf)
	if err != nil {
		t.Fatalf("ReadMongoCommand: %v", err)
	}
	if cmd.OpCode != OpQuery {
		t.Errorf("OpCode = %d, want OpQuery", cmd.OpCode)
	}
	if cmd.DB != "admin" {
		t.Errorf("DB = %q, want admin (from admin.$cmd)", cmd.DB)
	}
	if cmd.RequestID != 11 {
		t.Errorf("RequestID = %d, want 11", cmd.RequestID)
	}
	if !IsHelloCommand(cmd.Doc) {
		t.Error("decoded OP_QUERY doc not recognized as hello")
	}
}

func TestReadMongoCommand_RejectsUnexpectedOpcode(t *testing.T) {
	// An OP_REPLY (opcode 1) is never sent by a client; reading one as a
	// command must error rather than silently succeed.
	var buf bytes.Buffer
	if err := WriteMongoMessage(&buf, 1, 0, OpReply, EncodeOpReplyBody(0, 0, 0, nil)); err != nil {
		t.Fatalf("WriteMongoMessage: %v", err)
	}
	if _, err := ReadMongoCommand(&buf); err == nil {
		t.Error("expected error for OP_REPLY command, got nil")
	}
}

func TestWriteMongoReply_OpMsg(t *testing.T) {
	reply := BSON{{Key: "ok", Value: 1.0}, {Key: "marker", Value: int32(99)}}
	var buf bytes.Buffer
	if err := WriteMongoReply(&buf, OpMsg, 7, reply); err != nil {
		t.Fatalf("WriteMongoReply: %v", err)
	}

	hdr, body, err := ReadMongoMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMongoMessage: %v", err)
	}
	if hdr.OpCode != OpMsg {
		t.Errorf("reply opcode = %d, want OpMsg", hdr.OpCode)
	}
	if hdr.ResponseTo != 7 {
		t.Errorf("ResponseTo = %d, want 7", hdr.ResponseTo)
	}
	_, got, err := ParseOpMsg(body)
	if err != nil {
		t.Fatalf("ParseOpMsg: %v", err)
	}
	if !reflect.DeepEqual(got, reply) {
		t.Errorf("reply doc mismatch:\n got %#v\nwant %#v", got, reply)
	}
}

func TestWriteMongoReply_OpReply(t *testing.T) {
	reply := BSON{{Key: "ismaster", Value: true}, {Key: "ok", Value: 1.0}}
	var buf bytes.Buffer
	if err := WriteMongoReply(&buf, OpQuery, 11, reply); err != nil {
		t.Fatalf("WriteMongoReply: %v", err)
	}

	hdr, body, err := ReadMongoMessage(&buf)
	if err != nil {
		t.Fatalf("ReadMongoMessage: %v", err)
	}
	if hdr.OpCode != OpReply {
		t.Errorf("reply opcode = %d, want OpReply", hdr.OpCode)
	}
	if hdr.ResponseTo != 11 {
		t.Errorf("ResponseTo = %d, want 11", hdr.ResponseTo)
	}
	// OP_REPLY body: responseFlags(4)+cursorID(8)+startingFrom(4)+numberReturned(4) = 20.
	if n := int32(binary.LittleEndian.Uint32(body[16:20])); n != 1 {
		t.Fatalf("numberReturned = %d, want 1", n)
	}
	got, _, err := ReadBSONDocument(body[20:])
	if err != nil {
		t.Fatalf("ReadBSONDocument: %v", err)
	}
	if !reflect.DeepEqual(got, reply) {
		t.Errorf("reply doc mismatch:\n got %#v\nwant %#v", got, reply)
	}
}

// TestHandshakeRoundTrip exercises the full responder over a net.Pipe: a fake
// client sends an OP_MSG hello, the "proxy" side reads it with ReadMongoCommand,
// recognizes the probe, and answers with BuildHelloReply via WriteMongoReply.
func TestHandshakeRoundTrip(t *testing.T) {
	clientConn, proxyConn := net.Pipe()
	_ = clientConn.SetDeadline(time.Now().Add(5 * time.Second))
	_ = proxyConn.SetDeadline(time.Now().Add(5 * time.Second))

	const reqID int32 = 1234
	helloReply := make(chan BSON, 1)
	go func() {
		defer clientConn.Close()
		hello := BSON{{Key: "hello", Value: int32(1)}, {Key: "$db", Value: "admin"}}
		if err := WriteMongoMessage(clientConn, reqID, 0, OpMsg, EncodeOpMsgBody(0, hello)); err != nil {
			t.Errorf("client write hello: %v", err)
			return
		}
		hdr, body, err := ReadMongoMessage(clientConn)
		if err != nil {
			t.Errorf("client read reply: %v", err)
			return
		}
		if hdr.ResponseTo != reqID {
			t.Errorf("reply ResponseTo = %d, want %d", hdr.ResponseTo, reqID)
		}
		_, doc, err := ParseOpMsg(body)
		if err != nil {
			t.Errorf("client ParseOpMsg: %v", err)
			return
		}
		helloReply <- doc
	}()

	cmd, err := ReadMongoCommand(proxyConn)
	if err != nil {
		t.Fatalf("ReadMongoCommand: %v", err)
	}
	if !IsHelloCommand(cmd.Doc) {
		t.Fatalf("expected hello, got command %q", cmd.CommandName())
	}
	if err := WriteMongoReply(proxyConn, cmd.OpCode, cmd.RequestID, BuildHelloReply(1, time.Now())); err != nil {
		t.Fatalf("WriteMongoReply: %v", err)
	}
	_ = proxyConn.Close()

	got := <-helloReply
	if v, _ := got.Lookup("ok"); v != 1.0 {
		t.Errorf("client received ok = %#v, want 1.0", v)
	}
	if _, ok := got.Lookup("saslSupportedMechs"); ok {
		t.Error("client received saslSupportedMechs; proxy must advertise auth-disabled")
	}
}
