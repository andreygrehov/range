package nbd

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
)

func nbdSendRequest(t *testing.T, conn net.Conn, command uint16, handle uint64, offset int64, length uint32) {
	t.Helper()
	frame := make([]byte, 0, 28)
	frame = binary.BigEndian.AppendUint32(frame, requestMagic)
	frame = binary.BigEndian.AppendUint16(frame, 0)
	frame = binary.BigEndian.AppendUint16(frame, command)
	frame = binary.BigEndian.AppendUint64(frame, handle)
	frame = binary.BigEndian.AppendUint64(frame, uint64(offset))
	frame = binary.BigEndian.AppendUint32(frame, length)
	if _, err := conn.Write(frame); err != nil {
		t.Fatalf("send nbd request: %v", err)
	}
}

func nbdReadReply(t *testing.T, conn net.Conn, payload int) (handle uint64, code uint32, data []byte) {
	t.Helper()
	header := make([]byte, 16)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("read nbd reply: %v", err)
	}
	if got := binary.BigEndian.Uint32(header[0:4]); got != simpleReplyMagic {
		t.Fatalf("reply magic = %#x, want %#x", got, simpleReplyMagic)
	}
	code = binary.BigEndian.Uint32(header[4:8])
	handle = binary.BigEndian.Uint64(header[8:16])
	if code == 0 && payload > 0 {
		data = make([]byte, payload)
		if _, err := io.ReadFull(conn, data); err != nil {
			t.Fatalf("read nbd payload: %v", err)
		}
	}
	return handle, code, data
}
