// Package gzindex reads a gzip stream from the middle.
//
// DEFLATE cannot be entered at an arbitrary byte: every block may refer back
// 32 KiB into output that came before it, and blocks do not start on byte
// boundaries. Build decompresses a stream once and records a checkpoint every
// span of output: the exact bit where a block begins, how much output precedes
// it, and the 32 KiB window a decompressor needs from there. With the
// checkpoints, any range of the output costs one ranged read of the
// compressed bytes around it and at most one span of decompression.
//
// Build also hashes the compressed stream in fixed chunks, so bytes fetched
// later, from a store nobody here controls, are checked before they are used.
package gzindex

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
)

// Defaults for Build.
const (
	DefaultSpan  = 4 << 20  // output between checkpoints
	DefaultChunk = 64 << 10 // compressed bytes per hash
)

// lookahead is how far past the last byte it needs a decompressor may read
// while it finishes a block. Reads fetch this much more.
const lookahead = 8

// Checkpoint is a place decompression can start.
type Checkpoint struct {
	In     int64  // bit offset in the compressed stream
	Out    int64  // bytes of output before it
	Member bool   // In is the start of a gzip member header, byte aligned
	Window []byte // the output's last 32 KiB before Out; empty for a member
}

// Index is what Build learns about one gzip stream.
type Index struct {
	CompressedSize   int64
	UncompressedSize int64
	Chunk            int64
	ChunkHashes      [][sha256.Size]byte
	Checkpoints      []Checkpoint
}

// Fetch returns length bytes of the compressed stream from offset.
type Fetch func(ctx context.Context, offset, length int64) ([]byte, error)

// Build decompresses the gzip stream r. It returns the output as a reader,
// which the caller must read to the end, and a function that waits for that
// and returns the index.
func Build(r io.Reader, span, chunk int64) (io.Reader, func() (*Index, error)) {
	if span <= 0 {
		span = DefaultSpan
	}
	if chunk <= 0 {
		chunk = DefaultChunk
	}
	pr, pw := io.Pipe()
	idx := &Index{Chunk: chunk}
	done := make(chan error, 1)
	go func() {
		src := &streamReader{r: r, chunk: chunk, sum: sha256.New()}
		last := int64(-span)
		err := walk(src, Checkpoint{Member: true}, func(d *decompressor, start int64, out int64) {
			// A block boundary: record it if a span has passed.
			d.onBlock = func(bits, produced int64) {
				if out+produced-last < span {
					return
				}
				last = out + produced
				idx.Checkpoints = append(idx.Checkpoints, Checkpoint{
					In: start*8 + bits, Out: out + produced, Window: d.window(),
				})
			}
		}, func(member int64, out int64) {
			if out-last >= span {
				last = out
				idx.Checkpoints = append(idx.Checkpoints, Checkpoint{In: member * 8, Out: out, Member: true})
			}
		}, func(p []byte) error {
			idx.UncompressedSize += int64(len(p))
			_, err := pw.Write(p)
			return err
		})
		if err == nil {
			src.finish()
			idx.CompressedSize = src.pos
			idx.ChunkHashes = src.hashes
		}
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

// ReadAt fills p with output from off, fetching only the compressed bytes
// between the checkpoints around it and checking each chunk of them.
func (idx *Index) ReadAt(ctx context.Context, fetch Fetch, p []byte, off int64) error {
	if off < 0 || off+int64(len(p)) > idx.UncompressedSize {
		return fmt.Errorf("gzindex: range %d+%d outside %d bytes of output", off, len(p), idx.UncompressedSize)
	}
	if len(p) == 0 {
		return nil
	}
	end := off + int64(len(p))
	// The last checkpoint at or before off, and the compressed bytes from it
	// to the first checkpoint at or past end.
	i := sort.Search(len(idx.Checkpoints), func(i int) bool { return idx.Checkpoints[i].Out > off }) - 1
	cp := idx.Checkpoints[i]
	from := cp.In / 8
	to := idx.CompressedSize
	if j := sort.Search(len(idx.Checkpoints), func(j int) bool { return idx.Checkpoints[j].Out >= end }); j < len(idx.Checkpoints) {
		to = min(idx.CompressedSize, (idx.Checkpoints[j].In+7)/8+lookahead)
	}
	data, err := Verified(fetch, idx.CompressedSize, idx.Chunk, idx.ChunkHashes)(ctx, from, to-from)
	if err != nil {
		return err
	}

	src := &sliceReader{data: data, base: from, pos: from}
	errStop := errors.New("stop")
	pos := cp.Out
	filled := 0
	err = walk(src, cp, nil, nil, func(out []byte) error {
		// out covers [pos, pos+len(out)); copy the part inside [off, end).
		if s, e := max(pos, off), min(pos+int64(len(out)), end); s < e {
			filled += copy(p[s-off:], out[s-pos:e-pos])
		}
		pos += int64(len(out))
		if pos >= end {
			return errStop
		}
		return nil
	})
	// The fetch stops a few bytes past the last checkpoint needed. The
	// decompressor may want more before it hands out the last of the output,
	// and when the bytes run out it hands out what it holds and then fails;
	// by then p is full.
	if filled == len(p) {
		return nil
	}
	if err != nil && !errors.Is(err, errStop) {
		return err
	}
	return fmt.Errorf("gzindex: produced %d of %d bytes", filled, len(p))
}

// source is the compressed stream as walk consumes it.
type source interface {
	flateReader
	position() int64
	seek(pos int64) error // only ever backwards by a few bytes, or forwards
}

// walk decompresses from cp to the end of the stream, crossing gzip members,
// handing output to emit. started is called with each new decompressor, the
// byte it starts at and the output before it; member with each member header
// after the first.
func walk(src source, cp Checkpoint, started func(d *decompressor, start, out int64),
	member func(at, out int64), emit func([]byte) error) error {
	out := cp.Out
	window := cp.Window
	var skip uint
	if cp.Member {
		if err := src.seek(cp.In / 8); err != nil {
			return err
		}
		if err := readHeader(src); err != nil {
			return err
		}
		window = nil
	} else {
		if err := src.seek(cp.In / 8); err != nil {
			return err
		}
		skip = uint(cp.In % 8)
	}
	for {
		start := src.position()
		d, err := newDecompressor(src, window, skip)
		if err != nil {
			return err
		}
		if started != nil {
			started(d, start, out)
		}
		buf := make([]byte, 64<<10)
		for {
			n, err := d.Read(buf)
			if n > 0 {
				out += int64(n)
				if err := emit(buf[:n]); err != nil {
					return err
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		// The member's DEFLATE data ends at a bit; its 8-byte trailer starts
		// at the next byte. The decompressor may have read a little past it.
		if err := src.seek(start + (d.bitsUsed()+7)/8 + 8); err != nil {
			return err
		}
		at := src.position()
		if err := readHeader(src); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, errNotGzip) {
				return nil // the end, or padding after the last member
			}
			return err
		}
		if member != nil {
			member(at, out)
		}
		window, skip = nil, 0
	}
}

var errNotGzip = errors.New("gzindex: not a gzip member")

// readHeader consumes one gzip member header (RFC 1952, section 2.3).
func readHeader(r io.ByteReader) error {
	var head [10]byte
	for i := range head {
		c, err := r.ReadByte()
		if err != nil {
			if i == 0 {
				return io.EOF
			}
			return io.ErrUnexpectedEOF
		}
		head[i] = c
	}
	if head[0] != 0x1f || head[1] != 0x8b {
		return errNotGzip
	}
	if head[2] != 8 {
		return fmt.Errorf("gzindex: compression method %d is not DEFLATE", head[2])
	}
	flags := head[3]
	skipN := func(n int) error {
		for ; n > 0; n-- {
			if _, err := r.ReadByte(); err != nil {
				return io.ErrUnexpectedEOF
			}
		}
		return nil
	}
	skipString := func() error {
		for {
			c, err := r.ReadByte()
			if err != nil {
				return io.ErrUnexpectedEOF
			}
			if c == 0 {
				return nil
			}
		}
	}
	if flags&0x04 != 0 { // FEXTRA
		lo, err1 := r.ReadByte()
		hi, err2 := r.ReadByte()
		if err1 != nil || err2 != nil {
			return io.ErrUnexpectedEOF
		}
		if err := skipN(int(lo) | int(hi)<<8); err != nil {
			return err
		}
	}
	if flags&0x08 != 0 { // FNAME
		if err := skipString(); err != nil {
			return err
		}
	}
	if flags&0x10 != 0 { // FCOMMENT
		if err := skipString(); err != nil {
			return err
		}
	}
	if flags&0x02 != 0 { // FHCRC
		return skipN(2)
	}
	return nil
}

// streamReader reads the compressed stream once, front to back, hashing each
// chunk as it passes. It keeps the last few bytes so walk can step back over
// what a decompressor read past the end of a member.
type streamReader struct {
	r      io.Reader
	buf    []byte // read from r, not yet consumed
	pos    int64  // stream offset of the next byte handed out
	read   int64  // stream offset of the end of buf
	recent []byte // bytes before pos, for stepping back
	chunk  int64
	sum    hash.Hash
	hashed int64 // bytes fed to sum so far
	hashes [][sha256.Size]byte
}

func (s *streamReader) position() int64 { return s.pos }

func (s *streamReader) fill() error {
	if len(s.buf) > 0 {
		return nil
	}
	chunk := make([]byte, 256<<10)
	n, err := s.r.Read(chunk)
	if n > 0 {
		s.buf = chunk[:n]
		s.read += int64(n)
		s.hash(chunk[:n])
		return nil
	}
	if err == nil {
		err = io.ErrNoProgress
	}
	return err
}

// hash feeds bytes read from r into the chunk hashes.
func (s *streamReader) hash(p []byte) {
	for len(p) > 0 {
		room := s.chunk - s.hashed%s.chunk
		n := min(int64(len(p)), room)
		s.sum.Write(p[:n])
		s.hashed += n
		p = p[n:]
		if s.hashed%s.chunk == 0 {
			s.close()
		}
	}
}

func (s *streamReader) close() {
	var h [sha256.Size]byte
	copy(h[:], s.sum.Sum(nil))
	s.hashes = append(s.hashes, h)
	s.sum.Reset()
}

// finish drains r, so the hashes and size cover the whole stream, and closes
// the last partial chunk.
func (s *streamReader) finish() {
	s.pos += int64(len(s.buf))
	s.buf = nil
	for s.fill() == nil {
		s.pos += int64(len(s.buf))
		s.buf = nil
	}
	if s.hashed%s.chunk != 0 {
		s.close()
	}
}

func (s *streamReader) ReadByte() (byte, error) {
	if err := s.fill(); err != nil {
		return 0, err
	}
	c := s.buf[0]
	s.buf = s.buf[1:]
	s.pos++
	s.remember(c)
	return c, nil
}

func (s *streamReader) Read(p []byte) (int, error) {
	if err := s.fill(); err != nil {
		return 0, err
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	s.pos += int64(n)
	for _, c := range p[max(0, n-lookahead):n] {
		s.remember(c)
	}
	return n, nil
}

func (s *streamReader) remember(c byte) {
	s.recent = append(s.recent, c)
	if len(s.recent) > 4*lookahead {
		s.recent = s.recent[len(s.recent)-lookahead:]
	}
}

func (s *streamReader) seek(pos int64) error {
	for pos > s.pos {
		if _, err := s.ReadByte(); err != nil {
			return err
		}
	}
	back := s.pos - pos
	if back > int64(len(s.recent)) {
		return fmt.Errorf("gzindex: cannot step back %d bytes in a stream", back)
	}
	if back > 0 {
		kept := s.recent[len(s.recent)-int(back):]
		s.buf = append(append([]byte(nil), kept...), s.buf...)
		s.recent = s.recent[:len(s.recent)-int(back)]
		s.pos = pos
	}
	return nil
}

// sliceReader is a fetched range of the compressed stream.
type sliceReader struct {
	data []byte
	base int64 // stream offset of data[0]
	pos  int64
}

func (s *sliceReader) position() int64 { return s.pos }

func (s *sliceReader) seek(pos int64) error {
	if pos < s.base || pos > s.base+int64(len(s.data)) {
		return fmt.Errorf("gzindex: offset %d is outside the fetched range", pos)
	}
	s.pos = pos
	return nil
}

func (s *sliceReader) ReadByte() (byte, error) {
	if s.pos >= s.base+int64(len(s.data)) {
		return 0, io.EOF
	}
	c := s.data[s.pos-s.base]
	s.pos++
	return c, nil
}

func (s *sliceReader) Read(p []byte) (int, error) {
	rest := s.data[s.pos-s.base:]
	if len(rest) == 0 {
		return 0, io.EOF
	}
	n := copy(p, rest)
	s.pos += int64(n)
	return n, nil
}
