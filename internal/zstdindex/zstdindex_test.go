package zstdindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math/rand"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func testData(seed int64, n int) []byte {
	rng := rand.New(rand.NewSource(seed))
	var b bytes.Buffer
	words := []string{"range ", "layer ", "zstd ", "frame ", "\n", "window "}
	for b.Len() < n {
		if rng.Intn(4) == 0 {
			chunk := make([]byte, rng.Intn(32<<10))
			rng.Read(chunk)
			b.Write(chunk)
			continue
		}
		for i := rng.Intn(5000); i > 0; i-- {
			b.WriteString(words[rng.Intn(len(words))])
		}
	}
	return b.Bytes()[:n]
}

// oneFrame is what docker buildx writes: the whole layer in one frame.
func oneFrame(t *testing.T, data []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out, append([]zstd.EOption{zstd.WithEncoderConcurrency(1)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	w.Close()
	return out.Bytes()
}

// manyFrames is zstd:chunked-like: independent frames, with a skippable
// frame between two of them.
func manyFrames(t *testing.T, parts ...[]byte) []byte {
	t.Helper()
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderCRC(true))
	var out []byte
	for i, part := range parts {
		out = enc.EncodeAll(part, out)
		if i == 0 {
			skip := make([]byte, 8+13)
			binary.LittleEndian.PutUint32(skip, skippableMagic|3)
			binary.LittleEndian.PutUint32(skip[4:], 13)
			out = append(out, skip...)
		}
	}
	return out
}

func buildIndex(t *testing.T, compressed []byte) (*Index, []byte) {
	t.Helper()
	out, wait := Build(bytes.NewReader(compressed))
	got, err := io.ReadAll(out)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := wait()
	if err != nil {
		t.Fatal(err)
	}
	return idx, got
}

type counter struct {
	mu    sync.Mutex
	bytes int64
}

func (c *counter) fetch(compressed []byte) Fetch {
	return func(_ context.Context, off, n int64) ([]byte, error) {
		c.mu.Lock()
		c.bytes += n
		c.mu.Unlock()
		return compressed[off : off+n], nil
	}
}

func TestBuildSplitsFramesExactly(t *testing.T) {
	parts := [][]byte{testData(1, 300<<10), testData(2, 1<<20), testData(3, 5000)}
	compressed := manyFrames(t, parts...)
	idx, got := buildIndex(t, compressed)
	if want := bytes.Join(parts, nil); !bytes.Equal(got, want) {
		t.Fatal("Build output differs from the input")
	}
	if len(idx.Frames) != 4 || !idx.Frames[1].Skippable {
		t.Fatalf("frames %+v, want data, skippable, data, data", idx.Frames)
	}
	var in int64
	for _, f := range idx.Frames {
		if f.In != in {
			t.Fatalf("frame at %d, want %d: frames must tile the stream", f.In, in)
		}
		in += f.InLen
	}
	if in != int64(len(compressed)) || idx.CompressedSize != in {
		t.Fatalf("frames cover %d of %d bytes", in, len(compressed))
	}
}

func TestSmallFramesDecodeOnlyWhatIsRead(t *testing.T) {
	var parts [][]byte
	for i := 0; i < 20; i++ {
		parts = append(parts, testData(int64(10+i), 200<<10))
	}
	all := bytes.Join(parts, nil)
	compressed := manyFrames(t, parts...)
	idx, _ := buildIndex(t, compressed)
	var c counter
	r, err := NewReader(idx, c.fetch(compressed), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	off := int64(15*200<<10 + 1234)
	p := make([]byte, 5000)
	if err := r.ReadAt(context.Background(), p, off); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, all[off:off+5000]) {
		t.Fatal("wrong bytes from a small frame")
	}
	var largest int64
	for _, f := range idx.Frames {
		largest = max(largest, f.InLen)
	}
	if c.bytes > groupBytes+largest {
		t.Errorf("fetched %d of %d compressed bytes to read inside one frame, want one group", c.bytes, len(compressed))
	}
}

func TestOneLargeFrameDecodesForwardOnce(t *testing.T) {
	data := testData(4, 40<<20)
	for _, opts := range [][]zstd.EOption{nil, {zstd.WithEncoderCRC(false)}, {zstd.WithEncoderLevel(zstd.SpeedBestCompression)}} {
		compressed := oneFrame(t, data, opts...)
		idx, got := buildIndex(t, compressed)
		if !bytes.Equal(got, data) || len(idx.Frames) != 1 {
			t.Fatalf("one frame: output ok=%v, %d frames", bytes.Equal(got, data), len(idx.Frames))
		}
		var c counter
		r, err := NewReader(idx, c.fetch(compressed), t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		read := func(off, n int64) {
			t.Helper()
			p := make([]byte, n)
			if err := r.ReadAt(context.Background(), p, off); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(p, data[off:off+n]) {
				t.Fatalf("wrong bytes at %d", off)
			}
		}
		read(1000, 4096) // near the start: one step of input
		if c.bytes > step {
			t.Errorf("fetched %d bytes for a read near the start, want one step of %d", c.bytes, step)
		}
		read(30<<20, 1<<20) // forward
		fetched := c.bytes
		read(5<<20, 1<<20) // behind the front: from the spill file
		read(0, 100)
		if c.bytes != fetched {
			t.Errorf("a read behind the front fetched %d more bytes", c.bytes-fetched)
		}
		read(int64(len(data))-10, 10) // to the end
		if c.bytes > int64(len(compressed)) {
			t.Errorf("fetched %d bytes, more than the whole stream (%d)", c.bytes, len(compressed))
		}
	}
}

func TestConcurrentReadsOfOneFrame(t *testing.T) {
	data := testData(5, 30<<20)
	compressed := oneFrame(t, data)
	idx, _ := buildIndex(t, compressed)
	var c counter
	r, err := NewReader(idx, c.fetch(compressed), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := rand.New(rand.NewSource(int64(i))).Int63n(int64(len(data)) - 70000)
			p := make([]byte, 70000)
			if err := r.ReadAt(context.Background(), p, off); err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(p, data[off:off+70000]) {
				t.Errorf("wrong bytes at %d", off)
			}
		}(i)
	}
	wg.Wait()
	if c.bytes > int64(len(compressed)) {
		t.Errorf("fetched %d bytes for a %d-byte stream: something was fetched twice", c.bytes, len(compressed))
	}
}

func TestNotZstd(t *testing.T) {
	out, wait := Build(bytes.NewReader([]byte("definitely not zstd")))
	io.Copy(io.Discard, out)
	if _, err := wait(); err == nil {
		t.Fatal("built an index of something that is not zstd")
	}
}

// A read cancelled in the middle of a large frame leaves the decoder
// half-way. The next read starts a new one and must not misplace output.
func TestAnInterruptedFrontResumesCorrectly(t *testing.T) {
	data := testData(6, 24<<20)
	compressed := oneFrame(t, data)
	idx, _ := buildIndex(t, compressed)
	calls := 0
	fetch := func(ctx context.Context, off, n int64) ([]byte, error) {
		calls++
		if calls == 3 {
			return nil, context.Canceled
		}
		return compressed[off : off+n], nil
	}
	r, err := NewReader(idx, fetch, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 1<<20)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the failing fetch looks like a cancelled request
	_ = r.ReadAt(ctx, p, 20<<20)
	for _, off := range []int64{0, 3 << 20, 20 << 20, int64(len(data)) - (1 << 20)} {
		if err := r.ReadAt(context.Background(), p, off); err != nil {
			t.Fatalf("read at %d after an interruption: %v", off, err)
		}
		if !bytes.Equal(p, data[off:off+(1<<20)]) {
			t.Fatalf("wrong bytes at %d after an interruption", off)
		}
	}
}

// zstd:chunked has a frame per file. Neighbouring frames come in one request.
func TestSmallFramesAreFetchedInGroups(t *testing.T) {
	var parts [][]byte
	for i := 0; i < 400; i++ {
		parts = append(parts, testData(int64(100+i), 2000))
	}
	all := bytes.Join(parts, nil)
	compressed := manyFrames(t, parts...)
	idx, _ := buildIndex(t, compressed)
	calls := 0
	r, err := NewReader(idx, func(_ context.Context, off, n int64) ([]byte, error) {
		calls++
		return compressed[off : off+n], nil
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 400; k++ { // every file, one after another
		off := int64(k * 2000)
		p := make([]byte, 2000)
		if err := r.ReadAt(context.Background(), p, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(p, all[off:off+2000]) {
			t.Fatalf("file %d reads back wrong", k)
		}
	}
	if want := len(compressed)/groupBytes + 2; calls > want {
		t.Errorf("%d requests for 400 small frames in %d bytes, want at most %d", calls, len(compressed), want)
	}
}

// Reads of neighbouring files arrive together from the kernel. A run of small
// frames must still be fetched once.
func TestConcurrentReadsOfOneGroupFetchItOnce(t *testing.T) {
	var parts [][]byte
	for i := 0; i < 200; i++ {
		parts = append(parts, testData(int64(500+i), 3000))
	}
	all := bytes.Join(parts, nil)
	compressed := manyFrames(t, parts...)
	idx, _ := buildIndex(t, compressed)
	var c counter
	r, err := NewReader(idx, c.fetch(compressed), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for k := 0; k < 200; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			off := int64(k * 3000)
			p := make([]byte, 3000)
			if err := r.ReadAt(context.Background(), p, off); err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(p, all[off:off+3000]) {
				t.Errorf("file %d wrong", k)
			}
		}(k)
	}
	wg.Wait()
	if c.bytes > int64(len(compressed)) {
		t.Errorf("fetched %d bytes of a %d-byte stream: a group was fetched twice", c.bytes, len(compressed))
	}
}

// A network error while a large frame decodes costs a retry, not the layer.
func TestATransientFetchErrorIsRetried(t *testing.T) {
	data := testData(8, 24<<20)
	compressed := oneFrame(t, data)
	idx, _ := buildIndex(t, compressed)
	calls := 0
	r, err := NewReader(idx, func(_ context.Context, off, n int64) ([]byte, error) {
		calls++
		if calls == 2 {
			return nil, io.ErrUnexpectedEOF // a connection reset
		}
		return compressed[off : off+n], nil
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 1<<20)
	if err := r.ReadAt(context.Background(), p, 20<<20); err == nil {
		t.Fatal("the failing fetch did not surface")
	}
	if err := r.ReadAt(context.Background(), p, 20<<20); err != nil {
		t.Fatalf("the retry failed too: %v", err)
	}
	if !bytes.Equal(p, data[20<<20:21<<20]) {
		t.Fatal("wrong bytes after the retry")
	}
}
