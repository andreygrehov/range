package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// clientHandshake is the client side of the fixed newstyle negotiation,
// using NBD_OPT_GO for the default export. It leaves the connection in
// transmission phase, which is exactly what NBD_SET_SOCK expects, so the
// kernel can be handed the socket directly. This replaces nbd-client.
func clientHandshake(conn io.ReadWriter) (size int64, flags uint16, err error) {
	hello := make([]byte, 18)
	if _, err := io.ReadFull(conn, hello); err != nil {
		return 0, 0, fmt.Errorf("nbd: read greeting: %w", err)
	}
	if string(hello[0:8]) != "NBDMAGIC" || binary.BigEndian.Uint64(hello[8:16]) != optionMagic {
		return 0, 0, errors.New("nbd: server is not a newstyle NBD server")
	}
	serverFlags := binary.BigEndian.Uint16(hello[16:18])
	if serverFlags&flagFixedNewstyle == 0 {
		return 0, 0, errors.New("nbd: server does not offer fixed newstyle negotiation")
	}
	clientFlags := uint32(flagFixedNewstyle)
	if serverFlags&flagNoZeroes != 0 {
		clientFlags |= flagNoZeroes
	}
	if err := binary.Write(conn, binary.BigEndian, clientFlags); err != nil {
		return 0, 0, err
	}
	// NBD_OPT_GO for the default (empty-named) export, requesting no extra
	// information: the server sends NBD_INFO_EXPORT regardless.
	option := make([]byte, 0, 22)
	option = binary.BigEndian.AppendUint64(option, optionMagic)
	option = binary.BigEndian.AppendUint32(option, optGo)
	option = binary.BigEndian.AppendUint32(option, 6)
	option = binary.BigEndian.AppendUint32(option, 0) // export name length
	option = binary.BigEndian.AppendUint16(option, 0) // information requests
	if _, err := conn.Write(option); err != nil {
		return 0, 0, err
	}
	haveExport := false
	for {
		head := make([]byte, 20)
		if _, err := io.ReadFull(conn, head); err != nil {
			return 0, 0, fmt.Errorf("nbd: read option reply: %w", err)
		}
		if binary.BigEndian.Uint64(head[0:8]) != optionReplyMagic {
			return 0, 0, errors.New("nbd: bad option reply magic")
		}
		replyType := binary.BigEndian.Uint32(head[12:16])
		length := binary.BigEndian.Uint32(head[16:20])
		if length > 4096 {
			return 0, 0, fmt.Errorf("nbd: option reply too large (%d)", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return 0, 0, err
		}
		switch {
		case replyType == repAck:
			if !haveExport {
				return 0, 0, errors.New("nbd: server acknowledged without describing the export")
			}
			return size, flags, nil
		case replyType == repInfo:
			if len(payload) >= 12 && binary.BigEndian.Uint16(payload[0:2]) == 0 {
				size = int64(binary.BigEndian.Uint64(payload[2:10]))
				flags = binary.BigEndian.Uint16(payload[10:12])
				haveExport = true
			}
		case replyType&(1<<31) != 0:
			return 0, 0, fmt.Errorf("nbd: server refused the export (error reply %#x)", replyType)
		}
	}
}
