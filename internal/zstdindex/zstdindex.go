// Package zstdindex reads a zstd stream from the middle, as far as zstd allows.
//
// A zstd stream is a sequence of frames, and each frame decodes on its own.
// Build splits a stream into frames without decoding it twice - the frame
// and block headers say how long everything is - decodes each one once, and
// records where every frame starts in the compressed stream and in the output.
//
// A small frame, like the per-file frames of zstd:chunked, is decoded whole the
// first time anything in it is read. A large frame cannot be entered in the
// middle: its blocks refer back up to the window before them. For those the
// Reader keeps a live decoder at the furthest point anything has been read,
// and writes everything it decodes to a sparse spill file. A read behind that
// front comes from the file; a read past it moves the decoder forward. Nothing
// is fetched or decoded twice, and nothing past the furthest read at all.
package zstdindex

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

// Frame is one frame of a stream.
type Frame struct {
	In, InLen   int64 // where it sits in the compressed stream
	Out, OutLen int64 // where its output sits in the decoded stream
	Skippable   bool  // carries no output
}

// Index is what Build learns about one zstd stream.
type Index struct {
	CompressedSize   int64
	UncompressedSize int64
	Frames           []Frame
}

// Build decodes the zstd stream r once. It returns the output as a reader,
// which the caller must read to the end, and a function that waits for that
// and returns the index.
func Build(r io.Reader) (io.Reader, func() (*Index, error)) {
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	idx := &Index{}
	go func() {
		err := build(r, pw, idx)
		pw.CloseWithError(err)
		done <- err
	}()
	return pr, func() (*Index, error) {
		if err := <-done; err != nil {
			return nil, err
		}
		return idx, nil
	}
}

func build(r io.Reader, out io.Writer, idx *Index) error {
	split := &splitter{r: bufio.NewReaderSize(r, 1<<20)}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(1<<31))
	if err != nil {
		return err
	}
	defer dec.Close()
	for {
		frame, body, err := split.next()
		if err == io.EOF {
			idx.CompressedSize = split.pos
			return nil
		}
		if err != nil {
			return err
		}
		frame.Out = idx.UncompressedSize
		if frame.Skippable {
			if _, err := io.Copy(io.Discard, body); err != nil {
				return err
			}
		} else {
			if err := dec.Reset(body); err != nil {
				return err
			}
			n, err := io.Copy(out, dec)
			if err != nil {
				return err
			}
			frame.OutLen = n
			// The decoder may stop at the last block and leave a checksum.
			if _, err := io.Copy(io.Discard, body); err != nil {
				return err
			}
		}
		frame.InLen = split.pos - frame.In
		idx.UncompressedSize += frame.OutLen
		idx.Frames = append(idx.Frames, frame)
	}
}

// splitter walks a zstd stream frame by frame, handing out each frame's
// bytes exactly, by reading its headers (RFC 8878, section 3.1).
type splitter struct {
	r   *bufio.Reader
	pos int64
}

var errNotZstd = errors.New("zstdindex: not a zstd frame")

const (
	frameMagic     = 0xFD2FB528
	skippableMagic = 0x184D2A50 // the low four bits are free
)

func (s *splitter) next() (Frame, io.Reader, error) {
	magic, err := s.r.Peek(4)
	if len(magic) == 0 && err != nil {
		return Frame{}, nil, io.EOF
	}
	if len(magic) < 4 {
		return Frame{}, nil, io.ErrUnexpectedEOF
	}
	start := s.pos
	m := binary.LittleEndian.Uint32(magic)
	switch {
	case m&0xFFFFFFF0 == skippableMagic:
		head := make([]byte, 8)
		if _, err := io.ReadFull(s.r, head); err != nil {
			return Frame{}, nil, err
		}
		s.pos += 8
		size := int64(binary.LittleEndian.Uint32(head[4:]))
		return Frame{In: start, Skippable: true}, &counting{s: s, r: io.LimitReader(s.r, size)}, nil
	case m == frameMagic:
		return Frame{In: start}, &frameBody{s: s}, nil
	}
	return Frame{}, nil, fmt.Errorf("%w at offset %d", errNotZstd, start)
}

// counting reads through the splitter, advancing its position.
type counting struct {
	s *splitter
	r io.Reader
}

func (c *counting) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.s.pos += int64(n)
	return n, err
}

// frameBody yields one frame's bytes: its header, each block with its
// header, and the checksum, then io.EOF.
type frameBody struct {
	s        *splitter
	pending  []byte // header bytes parsed but not yet handed out
	started  bool
	inBlock  int64 // block content bytes left
	last     bool  // the last block's header has been read
	checksum bool
	done     bool
}

func (f *frameBody) Read(p []byte) (int, error) {
	for {
		if len(f.pending) > 0 {
			n := copy(p, f.pending)
			f.pending = f.pending[n:]
			f.s.pos += int64(n)
			return n, nil
		}
		if f.inBlock > 0 {
			n, err := f.s.r.Read(p[:min(int64(len(p)), f.inBlock)])
			f.inBlock -= int64(n)
			f.s.pos += int64(n)
			if err == io.EOF && f.inBlock > 0 {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
		if f.done {
			return 0, io.EOF
		}
		if err := f.step(); err != nil {
			return 0, err
		}
	}
}

// step parses the next header of the frame into pending.
func (f *frameBody) step() error {
	if !f.started {
		f.started = true
		head, err := f.take(5) // magic and the frame header descriptor
		if err != nil {
			return err
		}
		fhd := head[4]
		fcsFlag, single, dictFlag := fhd>>6, fhd&0x20 != 0, fhd&0x03
		f.checksum = fhd&0x04 != 0
		if fhd&0x08 != 0 {
			return errors.New("zstdindex: reserved bit set in a frame header")
		}
		rest := 0
		if !single {
			rest++ // window descriptor
		}
		rest += []int{0, 1, 2, 4}[dictFlag]
		switch fcsFlag {
		case 0:
			if single {
				rest++
			}
		case 1:
			rest += 2
		case 2:
			rest += 4
		case 3:
			rest += 8
		}
		more, err := f.take(rest)
		if err != nil {
			return err
		}
		f.pending = append(head, more...)
		return nil
	}
	if f.last {
		f.done = true
		if f.checksum {
			sum, err := f.take(4)
			if err != nil {
				return err
			}
			f.pending = sum
		}
		return nil
	}
	head, err := f.take(3)
	if err != nil {
		return err
	}
	h := uint32(head[0]) | uint32(head[1])<<8 | uint32(head[2])<<16
	f.last = h&1 != 0
	size := int64(h >> 3)
	switch (h >> 1) & 3 {
	case 0, 2: // raw, compressed: size bytes of content
		f.inBlock = size
	case 1: // RLE: one byte, repeated size times
		f.inBlock = 1
	default:
		return errors.New("zstdindex: reserved block type")
	}
	f.pending = head
	return nil
}

// take reads n header bytes without counting them yet: they are counted when
// they are handed out.
func (f *frameBody) take(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(f.s.r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return b, nil
}
