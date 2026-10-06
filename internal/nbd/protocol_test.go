package nbd

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/core/coretest"
	"github.com/andreygrehov/range/internal/rangetest"
)

func TestNBDTransmission(t *testing.T) {
	data := rangetest.Data(256 << 10)
	r, _ := coretest.NewReader(t, data, func(c *core.Config) { c.BlockSize = 64 << 10 })

	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(server, r, r.Size()) }()
	defer client.Close()

	t.Run("read returns the artifact bytes", func(t *testing.T) {
		nbdSendRequest(t, client, cmdRead, 1, 70000, 4096)
		handle, code, payload := nbdReadReply(t, client, 4096)
		if handle != 1 || code != 0 {
			t.Fatalf("reply = (handle %d, code %d), want (1, 0)", handle, code)
		}
		if string(payload) != string(data[70000:74096]) {
			t.Fatal("nbd read returned wrong bytes")
		}
	})

	t.Run("write is refused on a read-only export", func(t *testing.T) {
		frame := make([]byte, 0, 28)
		frame = binary.BigEndian.AppendUint32(frame, requestMagic)
		frame = binary.BigEndian.AppendUint16(frame, 0)
		frame = binary.BigEndian.AppendUint16(frame, cmdWrite)
		frame = binary.BigEndian.AppendUint64(frame, 2)
		frame = binary.BigEndian.AppendUint64(frame, 0)
		frame = binary.BigEndian.AppendUint32(frame, 8)
		frame = append(frame, []byte("12345678")...)
		if _, err := client.Write(frame); err != nil {
			t.Fatalf("send write: %v", err)
		}
		handle, code, _ := nbdReadReply(t, client, 0)
		if handle != 2 || code != errPerm {
			t.Fatalf("write reply = (handle %d, code %d), want (2, %d)", handle, code, errPerm)
		}
	})

	t.Run("out of bounds read is rejected", func(t *testing.T) {
		nbdSendRequest(t, client, cmdRead, 3, r.Size()-10, 4096)
		handle, code, _ := nbdReadReply(t, client, 0)
		if handle != 3 || code != errInval {
			t.Fatalf("reply = (handle %d, code %d), want (3, %d)", handle, code, errInval)
		}
	})

	t.Run("flush succeeds", func(t *testing.T) {
		nbdSendRequest(t, client, cmdFlush, 4, 0, 0)
		handle, code, _ := nbdReadReply(t, client, 0)
		if handle != 4 || code != 0 {
			t.Fatalf("flush reply = (handle %d, code %d), want (4, 0)", handle, code)
		}
	})

	t.Run("disconnect ends the session cleanly", func(t *testing.T) {
		nbdSendRequest(t, client, cmdDisc, 5, 0, 0)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("serveNBD returned %v, want nil", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("serveNBD did not return after NBD_CMD_DISC")
		}
	})
}

func TestNBDConcurrentReadsAreCorrect(t *testing.T) {
	data := rangetest.Data(1 << 20)
	r, _ := coretest.NewReader(t, data, func(c *core.Config) { c.BlockSize = 64 << 10 })

	client, server := net.Pipe()
	go Serve(server, r, r.Size())
	defer client.Close()

	const requests = 12
	const length = 8192
	expected := make(map[uint64]int64, requests)
	for i := 0; i < requests; i++ {
		offset := int64(i) * 40000
		handle := uint64(i + 100)
		expected[handle] = offset
		nbdSendRequest(t, client, cmdRead, handle, offset, length)
	}
	for i := 0; i < requests; i++ {
		handle, code, payload := nbdReadReply(t, client, length)
		if code != 0 {
			t.Fatalf("reply for handle %d had error code %d", handle, code)
		}
		offset, ok := expected[handle]
		if !ok {
			t.Fatalf("unexpected handle %d", handle)
		}
		if string(payload) != string(data[offset:offset+length]) {
			t.Fatalf("handle %d (offset %d) returned wrong bytes", handle, offset)
		}
		delete(expected, handle)
	}
	if len(expected) != 0 {
		t.Fatalf("%d requests went unanswered", len(expected))
	}
}

func TestNBDHandshakeExportName(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	errs := make(chan error, 1)
	go func() { errs <- ServerHandshake(server, 1<<30) }()

	header := make([]byte, 18)
	if _, err := io.ReadFull(client, header); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if string(header[0:8]) != "NBDMAGIC" {
		t.Fatalf("greeting = %q, want NBDMAGIC", header[0:8])
	}
	if binary.BigEndian.Uint64(header[8:16]) != optionMagic {
		t.Fatal("bad option magic in greeting")
	}
	flags := binary.BigEndian.Uint16(header[16:18])
	if flags&flagFixedNewstyle == 0 {
		t.Fatal("server did not advertise fixed newstyle")
	}

	// Reply with client flags, then request the export by name.
	if _, err := client.Write(binary.BigEndian.AppendUint32(nil, flagNoZeroes)); err != nil {
		t.Fatal(err)
	}
	option := make([]byte, 0, 16)
	option = binary.BigEndian.AppendUint64(option, optionMagic)
	option = binary.BigEndian.AppendUint32(option, optExportName)
	option = binary.BigEndian.AppendUint32(option, 0)
	if _, err := client.Write(option); err != nil {
		t.Fatal(err)
	}

	reply := make([]byte, 10)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("read export reply: %v", err)
	}
	if size := binary.BigEndian.Uint64(reply[0:8]); size != 1<<30 {
		t.Fatalf("export size = %d, want %d", size, 1<<30)
	}
	transmission := binary.BigEndian.Uint16(reply[8:10])
	if transmission&flagReadOnly == 0 {
		t.Fatal("export should be advertised read-only")
	}
	if err := <-errs; err != nil {
		t.Fatalf("nbdHandshake: %v", err)
	}
}

func TestNBDHandshakeGo(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	errs := make(chan error, 1)
	const size = 100 << 30
	go func() { errs <- ServerHandshake(server, size) }()

	if _, err := io.ReadFull(client, make([]byte, 18)); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if _, err := client.Write(binary.BigEndian.AppendUint32(nil, flagFixedNewstyle)); err != nil {
		t.Fatal(err)
	}
	option := make([]byte, 0, 16)
	option = binary.BigEndian.AppendUint64(option, optionMagic)
	option = binary.BigEndian.AppendUint32(option, optGo)
	option = binary.BigEndian.AppendUint32(option, 0)
	if _, err := client.Write(option); err != nil {
		t.Fatal(err)
	}

	// First an NBD_REP_INFO carrying the export size, then an ACK.
	head := make([]byte, 20)
	if _, err := io.ReadFull(client, head); err != nil {
		t.Fatalf("read info reply: %v", err)
	}
	if binary.BigEndian.Uint64(head[0:8]) != optionReplyMagic {
		t.Fatal("bad option reply magic")
	}
	if got := binary.BigEndian.Uint32(head[12:16]); got != repInfo {
		t.Fatalf("reply type = %d, want NBD_REP_INFO (%d)", got, repInfo)
	}
	payload := make([]byte, binary.BigEndian.Uint32(head[16:20]))
	if _, err := io.ReadFull(client, payload); err != nil {
		t.Fatal(err)
	}
	if got := binary.BigEndian.Uint64(payload[2:10]); got != size {
		t.Fatalf("advertised size = %d, want %d", got, int64(size))
	}
	if _, err := io.ReadFull(client, head); err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if got := binary.BigEndian.Uint32(head[12:16]); got != repAck {
		t.Fatalf("reply type = %d, want NBD_REP_ACK (%d)", got, repAck)
	}
	if err := <-errs; err != nil {
		t.Fatalf("nbdHandshake: %v", err)
	}
}

// Apple's Virtualization.framework asks for INFO, with the block sizes, before
// GO. The server must describe the export, stay in negotiation, then go.
func TestNBDHandshakeInfoThenGo(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	errs := make(chan error, 1)
	const size = 1 << 30
	go func() { errs <- ServerHandshake(server, size) }()

	if _, err := io.ReadFull(client, make([]byte, 18)); err != nil {
		t.Fatalf("read greeting: %v", err)
	}
	if _, err := client.Write(binary.BigEndian.AppendUint32(nil, flagFixedNewstyle)); err != nil {
		t.Fatal(err)
	}
	send := func(opt uint32, payload []byte) {
		t.Helper()
		option := binary.BigEndian.AppendUint64(nil, optionMagic)
		option = binary.BigEndian.AppendUint32(option, opt)
		option = binary.BigEndian.AppendUint32(option, uint32(len(payload)))
		if _, err := client.Write(append(option, payload...)); err != nil {
			t.Fatal(err)
		}
	}
	// replies reads option replies up to the ACK, by info type.
	replies := func() map[uint16][]byte {
		t.Helper()
		got := map[uint16][]byte{}
		for {
			head := make([]byte, 20)
			if _, err := io.ReadFull(client, head); err != nil {
				t.Fatalf("read reply: %v", err)
			}
			payload := make([]byte, binary.BigEndian.Uint32(head[16:20]))
			if _, err := io.ReadFull(client, payload); err != nil {
				t.Fatal(err)
			}
			switch binary.BigEndian.Uint32(head[12:16]) {
			case repAck:
				return got
			case repInfo:
				got[binary.BigEndian.Uint16(payload)] = payload[2:]
			default:
				t.Fatalf("unexpected reply type %#x", binary.BigEndian.Uint32(head[12:16]))
			}
		}
	}
	// No export name, one request: NBD_INFO_BLOCK_SIZE.
	request := []byte{0, 0, 0, 0, 0, 1, 0, infoBlockSize}
	send(optInfo, request)
	info := replies()
	if got := binary.BigEndian.Uint64(info[infoExport]); got != size {
		t.Fatalf("advertised size = %d, want %d", got, size)
	}
	sizes := info[infoBlockSize]
	if len(sizes) != 12 || binary.BigEndian.Uint32(sizes[0:4]) != 1 || binary.BigEndian.Uint32(sizes[8:12]) != maxRequest {
		t.Fatalf("block sizes = %x", sizes)
	}
	select {
	case err := <-errs:
		t.Fatalf("the handshake ended after INFO: %v", err)
	default:
	}
	send(optGo, request)
	replies()
	if err := <-errs; err != nil {
		t.Fatalf("handshake: %v", err)
	}
}

// The Go client must negotiate with Range's own server exactly as nbd-client
// did, and leave the connection in transmission phase for the kernel.
func TestNBDClientHandshakeAgainstOwnServer(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	const size = 64 << 20
	errc := make(chan error, 1)
	go func() { errc <- ServerHandshake(server, size) }()
	gotSize, flags, err := clientHandshake(client)
	if err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	if gotSize != size {
		t.Errorf("size = %d, want %d", gotSize, size)
	}
	if flags&flagReadOnly == 0 {
		t.Error("the export must be advertised read-only")
	}
	// One read in transmission phase proves the stream is aligned for NBD_SET_SOCK.
	data := rangetest.Data(size)
	go Serve(server, bytes.NewReader(data), size)
	req := make([]byte, 28)
	binary.BigEndian.PutUint32(req[0:], requestMagic)
	binary.BigEndian.PutUint16(req[6:], cmdRead)
	binary.BigEndian.PutUint64(req[8:], 7)
	binary.BigEndian.PutUint64(req[16:], 4096)
	binary.BigEndian.PutUint32(req[24:], 512)
	if _, err := client.Write(req); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 16+512)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint32(reply[4:8]) != 0 {
		t.Fatalf("read failed with error %d", binary.BigEndian.Uint32(reply[4:8]))
	}
	if !bytes.Equal(reply[16:], data[4096:4096+512]) {
		t.Fatal("transmission-phase read returned the wrong bytes")
	}
}
