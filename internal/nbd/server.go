package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
)

// ServerHandshake performs the server side of the fixed newstyle negotiation.
// The kernel path (NBD_SET_SOCK) skips this and starts in transmission phase.
func ServerHandshake(conn net.Conn, size int64) error {
	transmissionFlags := uint16(flagHasFlags | flagReadOnly)
	header := make([]byte, 0, 18)
	header = append(header, []byte("NBDMAGIC")...)
	header = binary.BigEndian.AppendUint64(header, optionMagic)
	header = binary.BigEndian.AppendUint16(header, flagFixedNewstyle|flagNoZeroes)
	if _, err := conn.Write(header); err != nil {
		return err
	}
	clientFlags := make([]byte, 4)
	if _, err := io.ReadFull(conn, clientFlags); err != nil {
		return err
	}
	noZeroes := binary.BigEndian.Uint32(clientFlags)&flagNoZeroes != 0
	for {
		head := make([]byte, 16)
		if _, err := io.ReadFull(conn, head); err != nil {
			return err
		}
		if binary.BigEndian.Uint64(head[0:8]) != optionMagic {
			return errors.New("nbd: bad option magic")
		}
		option := binary.BigEndian.Uint32(head[8:12])
		length := binary.BigEndian.Uint32(head[12:16])
		if length > 4096 {
			return fmt.Errorf("nbd: option payload too large (%d)", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return err
		}
		switch option {
		case optExportName:
			reply := make([]byte, 0, 134)
			reply = binary.BigEndian.AppendUint64(reply, uint64(size))
			reply = binary.BigEndian.AppendUint16(reply, transmissionFlags)
			if !noZeroes {
				reply = append(reply, make([]byte, 124)...)
			}
			if _, err := conn.Write(reply); err != nil {
				return err
			}
			return nil
		case optInfo, optGo:
			// INFO describes the export, and GO does too, then starts the
			// transmission phase. Apple's Virtualization.framework asks for
			// INFO first, with the block sizes.
			info := make([]byte, 0, 12)
			info = binary.BigEndian.AppendUint16(info, infoExport)
			info = binary.BigEndian.AppendUint64(info, uint64(size))
			info = binary.BigEndian.AppendUint16(info, transmissionFlags)
			if err := writeOptionReply(conn, option, repInfo, info); err != nil {
				return err
			}
			if requestsInfo(payload, infoBlockSize) {
				sizes := make([]byte, 0, 14)
				sizes = binary.BigEndian.AppendUint16(sizes, infoBlockSize)
				sizes = binary.BigEndian.AppendUint32(sizes, 1)          // minimum
				sizes = binary.BigEndian.AppendUint32(sizes, 4096)       // preferred
				sizes = binary.BigEndian.AppendUint32(sizes, maxRequest) // maximum
				if err := writeOptionReply(conn, option, repInfo, sizes); err != nil {
					return err
				}
			}
			if err := writeOptionReply(conn, option, repAck, nil); err != nil {
				return err
			}
			if option == optGo {
				return nil
			}
		case optAbort:
			writeOptionReply(conn, option, repAck, nil)
			return errors.New("nbd: client aborted negotiation")
		case optList:
			if err := writeOptionReply(conn, option, repErrUnsup, nil); err != nil {
				return err
			}
		default:
			if err := writeOptionReply(conn, option, repErrUnsup, nil); err != nil {
				return err
			}
		}
	}
}

// requestsInfo reports whether an INFO or GO payload asks for an information
// type: a name, then a count of 16-bit requests.
func requestsInfo(payload []byte, kind uint16) bool {
	if len(payload) < 4 {
		return false
	}
	rest := payload[4:]
	nameLen := int(binary.BigEndian.Uint32(payload[:4]))
	if nameLen > len(rest)-2 {
		return false
	}
	rest = rest[nameLen:]
	count := int(binary.BigEndian.Uint16(rest[:2]))
	rest = rest[2:]
	for i := 0; i < count && 2*i+2 <= len(rest); i++ {
		if binary.BigEndian.Uint16(rest[2*i:]) == kind {
			return true
		}
	}
	return false
}

func writeOptionReply(w io.Writer, option, replyType uint32, payload []byte) error {
	head := make([]byte, 0, 20+len(payload))
	head = binary.BigEndian.AppendUint64(head, optionReplyMagic)
	head = binary.BigEndian.AppendUint32(head, option)
	head = binary.BigEndian.AppendUint32(head, replyType)
	head = binary.BigEndian.AppendUint32(head, uint32(len(payload)))
	head = append(head, payload...)
	_, err := w.Write(head)
	return err
}

// Serve runs the transmission phase: read requests, answer them concurrently,
// write replies under a mutex so they never interleave.
func Serve(conn io.ReadWriter, source io.ReaderAt, size int64) error {
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	workers := make(chan struct{}, maxWorkers)
	defer wg.Wait()

	reply := func(handle []byte, code uint32, data []byte) error {
		head := make([]byte, 0, 16)
		head = binary.BigEndian.AppendUint32(head, simpleReplyMagic)
		head = binary.BigEndian.AppendUint32(head, code)
		head = append(head, handle...)
		writeMu.Lock()
		defer writeMu.Unlock()
		if _, err := conn.Write(head); err != nil {
			return err
		}
		if len(data) > 0 {
			if _, err := conn.Write(data); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		header := make([]byte, 28)
		if _, err := io.ReadFull(conn, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if binary.BigEndian.Uint32(header[0:4]) != requestMagic {
			return errors.New("nbd: bad request magic")
		}
		command := binary.BigEndian.Uint16(header[6:8])
		handle := append([]byte(nil), header[8:16]...)
		offset := int64(binary.BigEndian.Uint64(header[16:24]))
		length := int64(binary.BigEndian.Uint32(header[24:28]))

		switch command {
		case cmdDisc:
			return nil
		case cmdFlush:
			if err := reply(handle, 0, nil); err != nil {
				return err
			}
		case cmdWrite:
			// Read-only export: drain the payload, then refuse.
			if length > 0 && length <= maxRequest {
				if _, err := io.CopyN(io.Discard, conn, length); err != nil {
					return err
				}
			}
			if err := reply(handle, errPerm, nil); err != nil {
				return err
			}
		case cmdTrim:
			if err := reply(handle, errPerm, nil); err != nil {
				return err
			}
		case cmdRead:
			if length <= 0 || length > maxRequest || offset < 0 || offset+length > size {
				if err := reply(handle, errInval, nil); err != nil {
					return err
				}
				continue
			}
			workers <- struct{}{}
			wg.Add(1)
			go func(handle []byte, offset, length int64) {
				defer wg.Done()
				defer func() { <-workers }()
				buf := make([]byte, length)
				if _, err := source.ReadAt(buf, offset); err != nil && !errors.Is(err, io.EOF) {
					log.Printf("nbd read %d+%d: %v", offset, length, err)
					reply(handle, errIO, nil)
					return
				}
				reply(handle, 0, buf)
			}(handle, offset, length)
		default:
			if err := reply(handle, errInval, nil); err != nil {
				return err
			}
		}
	}
}
