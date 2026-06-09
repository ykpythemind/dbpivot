package proxy

import (
	"encoding/binary"
	"fmt"
	"io"
)

// MongoDB wire protocol framing.
//
// This file implements the MongoDB message envelope (the 16-byte msgHeader)
// and the three opcodes the handshake/auth phase needs: OP_MSG (2013, the
// modern command transport), plus OP_QUERY (2004) / OP_REPLY (1) which drivers
// still use for the very first `isMaster`/`hello` probe before they know the
// server's wire version. Command and reply bodies are BSON documents, encoded
// and decoded via bson.go.
//
// Like pgwire.go / mysqlwire.go these are pure functions over byte slices plus
// a pair of net.Conn-free read/write helpers, so they can be unit-tested in
// isolation and wired into the server later.
//
// Reference: MongoDB Wire Protocol
// (https://www.mongodb.com/docs/manual/reference/mongodb-wire-protocol/).

// MongoDB opcodes (subset used by the proxy).
const (
	OpReply = 1    // reply to an OP_QUERY (legacy)
	OpQuery = 2004 // legacy query, used for the initial handshake probe
	OpMsg   = 2013 // modern command/reply transport
)

// msgHeaderLen is the fixed size of every MongoDB message header.
const msgHeaderLen = 16

// MaxMessageSize caps an incoming message body so a malformed or hostile
// length field can't trigger an unbounded allocation. MongoDB's default
// maxMessageSizeBytes is 48 MiB; we allow a little headroom.
const MaxMessageSize = 64 * 1024 * 1024

// OP_MSG flag bits.
const (
	OpMsgFlagChecksumPresent = 1 << 0  // a trailing CRC-32C follows the sections
	OpMsgFlagMoreToCome      = 1 << 1  // no reply expected / more replies follow
	OpMsgFlagExhaustAllowed  = 1 << 16 // client supports exhaust cursors
)

// MsgHeader is the fixed 16-byte header prefixing every MongoDB message.
type MsgHeader struct {
	MessageLength int32 // total message size in bytes, including this header
	RequestID     int32
	ResponseTo    int32
	OpCode        int32
}

// ReadMongoMessage reads one MongoDB message: the 16-byte header followed by
// the body (MessageLength-16 bytes). The returned body excludes the header.
func ReadMongoMessage(r io.Reader) (MsgHeader, []byte, error) {
	var hdr [msgHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return MsgHeader{}, nil, err
	}
	h := MsgHeader{
		MessageLength: int32(binary.LittleEndian.Uint32(hdr[0:4])),
		RequestID:     int32(binary.LittleEndian.Uint32(hdr[4:8])),
		ResponseTo:    int32(binary.LittleEndian.Uint32(hdr[8:12])),
		OpCode:        int32(binary.LittleEndian.Uint32(hdr[12:16])),
	}
	if h.MessageLength < msgHeaderLen || int(h.MessageLength) > MaxMessageSize {
		return h, nil, fmt.Errorf("mongo message length out of range: %d", h.MessageLength)
	}
	body := make([]byte, int(h.MessageLength)-msgHeaderLen)
	if _, err := io.ReadFull(r, body); err != nil {
		return h, nil, err
	}
	return h, body, nil
}

// WriteMongoMessage writes a MongoDB message: a header (with MessageLength
// computed from the body) followed by body.
func WriteMongoMessage(w io.Writer, requestID, responseTo, opCode int32, body []byte) error {
	total := msgHeaderLen + len(body)
	var hdr [msgHeaderLen]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(total))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(requestID))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(responseTo))
	binary.LittleEndian.PutUint32(hdr[12:16], uint32(opCode))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

// EncodeOpMsgBody builds an OP_MSG body carrying a single kind-0 ("body")
// section — the form used for command requests and replies during the
// handshake/auth phase. It never sets the checksumPresent flag, so no CRC is
// appended.
func EncodeOpMsgBody(flags uint32, doc BSON) []byte {
	b := binary.LittleEndian.AppendUint32(nil, flags)
	b = append(b, 0) // section kind 0: body
	b = AppendBSON(b, doc)
	return b
}

// ParseOpMsg decodes an OP_MSG body, returning the flag bits and the kind-0
// body section's command document. Kind-1 document-sequence sections (used for
// batched writes) are skipped: they carry payload documents, never the command
// name, which always lives in the kind-0 body. A trailing CRC-32C is trimmed
// when the checksumPresent flag is set (its value is not verified).
func ParseOpMsg(body []byte) (flags uint32, doc BSON, err error) {
	if len(body) < 5 {
		return 0, nil, fmt.Errorf("op_msg: body too short (%d bytes)", len(body))
	}
	flags = binary.LittleEndian.Uint32(body[0:4])
	sections := body[4:]
	if flags&OpMsgFlagChecksumPresent != 0 {
		if len(sections) < 4 {
			return 0, nil, fmt.Errorf("op_msg: checksum flag set but body too short")
		}
		sections = sections[:len(sections)-4]
	}

	var found bool
	for len(sections) > 0 {
		kind := sections[0]
		sections = sections[1:]
		switch kind {
		case 0: // body: a single BSON document
			d, n, derr := ReadBSONDocument(sections)
			if derr != nil {
				return 0, nil, fmt.Errorf("op_msg: body section: %w", derr)
			}
			if found {
				return 0, nil, fmt.Errorf("op_msg: more than one body section")
			}
			doc = d
			found = true
			sections = sections[n:]
		case 1: // document sequence: int32 size | cstring identifier | docs
			if len(sections) < 4 {
				return 0, nil, fmt.Errorf("op_msg: truncated document-sequence size")
			}
			size := int(int32(binary.LittleEndian.Uint32(sections[0:4])))
			if size < 4 || size > len(sections) {
				return 0, nil, fmt.Errorf("op_msg: document-sequence size %d out of range", size)
			}
			sections = sections[size:] // size counts itself; skip the whole section
		default:
			return 0, nil, fmt.Errorf("op_msg: unknown section kind %d", kind)
		}
	}
	if !found {
		return 0, nil, fmt.Errorf("op_msg: no body section")
	}
	return flags, doc, nil
}

// ParseOpQuery decodes an OP_QUERY body. Drivers send this for the first
// handshake probe (on the "<db>.$cmd" collection) before they know the server
// supports OP_MSG. The optional returnFieldsSelector trailing document is
// ignored.
func ParseOpQuery(body []byte) (flags int32, fullCollection string, numberToSkip, numberToReturn int32, query BSON, err error) {
	if len(body) < 4 {
		return 0, "", 0, 0, nil, fmt.Errorf("op_query: body too short")
	}
	flags = int32(binary.LittleEndian.Uint32(body[0:4]))
	pos := 4
	fullCollection, pos, err = readCString(body, pos)
	if err != nil {
		return 0, "", 0, 0, nil, fmt.Errorf("op_query: collection: %w", err)
	}
	if pos+8 > len(body) {
		return 0, "", 0, 0, nil, fmt.Errorf("op_query: truncated skip/return")
	}
	numberToSkip = int32(binary.LittleEndian.Uint32(body[pos : pos+4]))
	numberToReturn = int32(binary.LittleEndian.Uint32(body[pos+4 : pos+8]))
	pos += 8
	query, _, err = ReadBSONDocument(body[pos:])
	if err != nil {
		return 0, "", 0, 0, nil, fmt.Errorf("op_query: query document: %w", err)
	}
	return flags, fullCollection, numberToSkip, numberToReturn, query, nil
}

// EncodeOpReplyBody builds an OP_REPLY body (the reply to an OP_QUERY).
// numberReturned is derived from len(docs).
func EncodeOpReplyBody(responseFlags int32, cursorID int64, startingFrom int32, docs []BSON) []byte {
	b := binary.LittleEndian.AppendUint32(nil, uint32(responseFlags))
	b = binary.LittleEndian.AppendUint64(b, uint64(cursorID))
	b = binary.LittleEndian.AppendUint32(b, uint32(startingFrom))
	b = binary.LittleEndian.AppendUint32(b, uint32(len(docs)))
	for _, d := range docs {
		b = AppendBSON(b, d)
	}
	return b
}
