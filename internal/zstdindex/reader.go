package zstdindex

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// Fetch returns length bytes of the compressed stream from offset, already
// checked.
type Fetch func(ctx context.Context, offset, length int64) ([]byte, error)

// wholeFrame is the largest frame decoded in one go the first time it is
// touched. Anything larger gets a decoder front.
const wholeFrame = 16 << 20

// step is how much compressed input a front fetches at a time.
const step = 4 << 20

// groupBytes bounds a run of small frames fetched and decoded together.
// zstd:chunked gives every file a frame of its own, often a few hundred bytes;
// one request per frame would cost a round trip per file, and neighbours in a
// layer are usually wanted together.
const groupBytes = 1 << 20

// Reader serves ranges of a stream's output. Decoded bytes go to a sparse
// spill file at their output offsets, so each is decoded once.
type Reader struct {
	idx   *Index
	fetch Fetch
	spill *os.File
	data  []int // indexes of the frames that carry output, in output order
	group []int // for each frame, the first frame of the run fetched with it

	mu     sync.Mutex
	frames map[int]*frameState
}

// frameState is how far one frame has been decoded.
type frameState struct {
	mu      sync.Mutex
	decoded int64 // output bytes of this frame in the spill file, from its start
	dec     *zstd.Decoder
	at      int64 // where dec is in the frame's output
	input   *sequential
}

// NewReader decodes into a spill file created in dir.
func NewReader(idx *Index, fetch Fetch, dir string) (*Reader, error) {
	spill, err := os.CreateTemp(dir, "range-zstd-*")
	if err != nil {
		return nil, err
	}
	// The file lives as long as the open descriptor: nothing to clean up
	// after the process, however it ends.
	os.Remove(spill.Name())
	r := &Reader{idx: idx, fetch: fetch, spill: spill, frames: map[int]*frameState{}}
	r.group = make([]int, len(idx.Frames))
	first, bytes := -1, int64(0)
	for i, f := range idx.Frames {
		if !f.Skippable && f.OutLen > 0 {
			r.data = append(r.data, i)
		}
		small := f.OutLen <= wholeFrame
		if !small || first < 0 || bytes+f.InLen > groupBytes {
			first, bytes = i, 0
		}
		r.group[i] = first
		bytes += f.InLen
		if !small {
			first = -1 // a large frame is alone
		}
	}
	return r, nil
}

// ReadAt fills p with output from off.
func (r *Reader) ReadAt(ctx context.Context, p []byte, off int64) error {
	frames := r.idx.Frames
	for len(p) > 0 {
		k := sort.Search(len(r.data), func(k int) bool {
			f := frames[r.data[k]]
			return f.Out+f.OutLen > off
		})
		if k == len(r.data) {
			return fmt.Errorf("zstdindex: offset %d is past the end of the output", off)
		}
		i := r.data[k]
		f := frames[i]
		end := min(f.Out+f.OutLen, off+int64(len(p)))
		if err := r.decodeTo(ctx, i, end-f.Out); err != nil {
			return err
		}
		n := end - off
		if _, err := r.spill.ReadAt(p[:n], off); err != nil {
			return err
		}
		p, off = p[n:], end
	}
	return nil
}

func (r *Reader) state(i int) *frameState {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.frames[i]
	if !ok {
		st = &frameState{}
		r.frames[i] = st
	}
	return st
}

// decodeTo makes sure frame i is decoded at least to want bytes of output.
func (r *Reader) decodeTo(ctx context.Context, i int, want int64) error {
	f := r.idx.Frames[i]
	if f.OutLen <= wholeFrame {
		return r.decodeGroup(ctx, i)
	}
	st := r.state(i)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.decoded >= want {
		return nil
	}
	// A large frame: keep one decoder at its front and move it forward.
	if st.dec == nil {
		st.input = &sequential{fetch: r.fetch, pos: f.In, end: f.In + f.InLen}
		dec, err := zstd.NewReader(st.input, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(1<<31))
		if err != nil {
			return err
		}
		// A decoder restarted after an interrupted read passes over what
		// the spill file already holds.
		st.dec, st.at = dec, 0
	}
	st.input.ctx = ctx
	buf := make([]byte, 1<<20)
	for st.decoded < want {
		n, err := st.dec.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if skip := st.decoded - st.at; skip > 0 {
				chunk = chunk[min(skip, int64(n)):]
			}
			st.at += int64(n)
			if len(chunk) > 0 {
				if _, werr := r.spill.WriteAt(chunk, f.Out+st.at-int64(len(chunk))); werr != nil {
					return werr
				}
				st.decoded = st.at
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			// The decoder cannot be rewound. A later read starts a new one,
			// which passes over what the spill file already holds, so a
			// network error costs a retry, not the layer.
			st.dec.Close()
			st.dec, st.input = nil, nil
			return err
		}
	}
	if st.decoded < want {
		return fmt.Errorf("zstdindex: frame at %d decoded to %d bytes, the index says %d", f.In, st.decoded, f.OutLen)
	}
	if st.decoded == f.OutLen && st.dec != nil {
		st.dec.Close()
		st.dec, st.input = nil, nil
	}
	return nil
}

// decodeGroup fetches the run of small frames that frame i belongs to in one
// request and decodes every frame of it. The run is locked as a whole - its
// first frame's lock stands for it - so reads of neighbouring files that
// arrive together fetch it once between them.
func (r *Reader) decodeGroup(ctx context.Context, i int) error {
	frames := r.idx.Frames
	first, last := r.group[i], i
	for last+1 < len(frames) && r.group[last+1] == first {
		last++
	}
	lock := r.state(first)
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if r.state(i).decoded >= frames[i].OutLen {
		return nil
	}
	lo, hi := frames[first].In, frames[last].In+frames[last].InLen
	data, err := r.fetch(ctx, lo, hi-lo)
	if err != nil {
		return err
	}
	for k := first; k <= last; k++ {
		f := frames[k]
		st := r.state(k)
		if f.Skippable || f.OutLen == 0 || st.decoded >= f.OutLen {
			continue
		}
		out, err := decodeAll(data[f.In-lo:f.In-lo+f.InLen], f.OutLen)
		if err != nil {
			return err
		}
		if _, err := r.spill.WriteAt(out, f.Out); err != nil {
			return err
		}
		st.decoded = f.OutLen
	}
	return nil
}

var decoders = sync.Pool{New: func() any {
	d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(1<<31))
	return d
}}

func decodeAll(frame []byte, size int64) ([]byte, error) {
	d := decoders.Get().(*zstd.Decoder)
	defer decoders.Put(d)
	out, err := d.DecodeAll(frame, make([]byte, 0, size))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) != size {
		return nil, fmt.Errorf("zstdindex: a frame decoded to %d bytes, the index says %d", len(out), size)
	}
	return out, nil
}

// sequential reads compressed input forward, a step at a time.
type sequential struct {
	fetch    Fetch
	ctx      context.Context
	pos, end int64
	buf      []byte
}

func (s *sequential) Read(p []byte) (int, error) {
	if len(s.buf) == 0 {
		if s.pos >= s.end {
			return 0, io.EOF
		}
		n := min(int64(step), s.end-s.pos)
		data, err := s.fetch(s.ctx, s.pos, n)
		if err != nil {
			return 0, err
		}
		s.buf, s.pos = data, s.pos+n
	}
	n := copy(p, s.buf)
	s.buf = s.buf[n:]
	return n, nil
}
