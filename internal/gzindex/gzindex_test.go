package gzindex

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"testing"
)

// testData mixes text-like runs, which compress, with random runs, which do
// not, so the stream has dynamic, fixed and stored blocks.
func testData(seed int64, n int) []byte {
	rng := rand.New(rand.NewSource(seed))
	words := []string{"range ", "shell ", "block ", "layer ", "gzip ", "\n", "kernel ", "erofs "}
	var b bytes.Buffer
	for b.Len() < n {
		if rng.Intn(4) == 0 {
			chunk := make([]byte, rng.Intn(64<<10))
			rng.Read(chunk)
			b.Write(chunk)
			continue
		}
		for i := rng.Intn(20000); i > 0; i-- {
			b.WriteString(words[rng.Intn(len(words))])
		}
	}
	return b.Bytes()[:n]
}

func gzipped(t *testing.T, level int, parts ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	for _, part := range parts { // one member per part
		w, err := gzip.NewWriterLevel(&out, level)
		if err != nil {
			t.Fatal(err)
		}
		w.Name = "member"
		w.Comment = "for the header parser"
		w.Extra = []byte{1, 2, 3}
		w.Write(part)
		w.Close()
	}
	return out.Bytes()
}

func build(t *testing.T, compressed []byte, span int64) (*Index, []byte) {
	t.Helper()
	out, wait := Build(bytes.NewReader(compressed), span, 4<<10)
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

func fetcher(compressed []byte, calls *int) Fetch {
	return func(_ context.Context, off, n int64) ([]byte, error) {
		if calls != nil {
			*calls++
		}
		return append([]byte(nil), compressed[off:off+n]...), nil
	}
}

func TestBuildReproducesTheStream(t *testing.T) {
	for _, level := range []int{gzip.NoCompression, gzip.HuffmanOnly, gzip.BestSpeed, gzip.DefaultCompression, gzip.BestCompression} {
		t.Run(fmt.Sprint(level), func(t *testing.T) {
			data := testData(int64(level+2), 3<<20)
			compressed := gzipped(t, level, data)
			idx, got := build(t, compressed, 256<<10)
			if !bytes.Equal(got, data) {
				t.Fatal("the output of Build differs from the input")
			}
			if idx.UncompressedSize != int64(len(data)) || idx.CompressedSize != int64(len(compressed)) {
				t.Fatalf("sizes %d/%d, want %d/%d", idx.UncompressedSize, idx.CompressedSize, len(data), len(compressed))
			}
			if len(idx.Checkpoints) < 4 {
				t.Errorf("%d checkpoints for 3 MiB at a 256 KiB span", len(idx.Checkpoints))
			}
			if want := (len(compressed) + 4<<10 - 1) / (4 << 10); len(idx.ChunkHashes) != want {
				t.Errorf("%d chunk hashes, want %d", len(idx.ChunkHashes), want)
			}
		})
	}
}

func TestReadAtAnyRange(t *testing.T) {
	data := testData(7, 5<<20)
	compressed := gzipped(t, gzip.DefaultCompression, data)
	idx, _ := build(t, compressed, 512<<10)
	rng := rand.New(rand.NewSource(1))
	cases := [][2]int64{{0, 1}, {0, 4096}, {int64(len(data)) - 1, 1}, {int64(len(data)) - 100000, 100000}, {0, int64(len(data))}}
	for i := 0; i < 200; i++ {
		off := rng.Int63n(int64(len(data)))
		n := 1 + rng.Int63n(min(int64(len(data))-off, 2<<20))
		cases = append(cases, [2]int64{off, n})
	}
	// Exactly at every checkpoint, and one byte either side.
	for _, cp := range idx.Checkpoints {
		for _, d := range []int64{-1, 0, 1} {
			if off := cp.Out + d; off >= 0 && off < int64(len(data)) {
				cases = append(cases, [2]int64{off, min(4096, int64(len(data))-off)})
			}
		}
	}
	for _, c := range cases {
		p := make([]byte, c[1])
		if err := idx.ReadAt(context.Background(), fetcher(compressed, nil), p, c[0]); err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", c[0], c[1], err)
		}
		if !bytes.Equal(p, data[c[0]:c[0]+c[1]]) {
			t.Fatalf("ReadAt(%d, %d) returned the wrong bytes", c[0], c[1])
		}
	}
}

func TestReadAtFetchesOnlyAroundTheRange(t *testing.T) {
	data := testData(9, 8<<20)
	compressed := gzipped(t, gzip.DefaultCompression, data)
	idx, _ := build(t, compressed, 512<<10)
	var fetched int64
	fetch := func(_ context.Context, off, n int64) ([]byte, error) {
		fetched += n
		return compressed[off : off+n], nil
	}
	p := make([]byte, 4096)
	if err := idx.ReadAt(context.Background(), fetch, p, 6<<20); err != nil {
		t.Fatal(err)
	}
	// One span of output, compressed, plus chunk rounding.
	if limit := int64(len(compressed))/8 + 3*idx.Chunk; fetched > limit {
		t.Errorf("fetched %d compressed bytes for 4 KiB of an 8 MiB stream, want at most %d", fetched, limit)
	}
}

func TestMultipleMembers(t *testing.T) {
	var parts [][]byte
	var all []byte
	for i := 0; i < 40; i++ { // like eStargz: many small members
		part := testData(int64(100+i), 1+rand.New(rand.NewSource(int64(i))).Intn(200<<10))
		parts = append(parts, part)
		all = append(all, part...)
	}
	compressed := gzipped(t, gzip.DefaultCompression, parts...)
	idx, got := build(t, compressed, 256<<10)
	if !bytes.Equal(got, all) {
		t.Fatal("Build output differs across members")
	}
	rng := rand.New(rand.NewSource(3))
	for i := 0; i < 100; i++ {
		off := rng.Int63n(int64(len(all)))
		n := 1 + rng.Int63n(min(int64(len(all))-off, 1<<20))
		p := make([]byte, n)
		if err := idx.ReadAt(context.Background(), fetcher(compressed, nil), p, off); err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", off, n, err)
		}
		if !bytes.Equal(p, all[off:off+n]) {
			t.Fatalf("ReadAt(%d, %d) wrong across members", off, n)
		}
	}
}

func TestTamperedBytesAreRefused(t *testing.T) {
	data := testData(11, 2<<20)
	compressed := gzipped(t, gzip.DefaultCompression, data)
	idx, _ := build(t, compressed, 256<<10)
	bad := append([]byte(nil), compressed...)
	bad[len(bad)/2] ^= 0xff
	p := make([]byte, 1000)
	err := idx.ReadAt(context.Background(), fetcher(bad, nil), p, int64(len(data))/2-500)
	if err == nil {
		t.Fatal("a flipped byte was accepted")
	}
}

func TestTrailingZerosAfterTheLastMember(t *testing.T) {
	data := testData(13, 1<<20)
	compressed := append(gzipped(t, gzip.DefaultCompression, data), make([]byte, 1024)...)
	idx, got := build(t, compressed, 256<<10)
	if !bytes.Equal(got, data) {
		t.Fatal("output differs")
	}
	if idx.CompressedSize != int64(len(compressed)) {
		t.Errorf("CompressedSize %d, want the whole stream %d", idx.CompressedSize, len(compressed))
	}
}

func TestNotGzip(t *testing.T) {
	out, wait := Build(bytes.NewReader([]byte("plain tar, not gzip")), 0, 0)
	io.Copy(io.Discard, out)
	if _, err := wait(); err == nil {
		t.Fatal("a stream that is not gzip built an index")
	}
}

// Starting at a member header, with no window, is how an index that
// checkpoints only member boundaries (eStargz, one member per file) is read.
func TestReadFromMemberCheckpoints(t *testing.T) {
	var compressed []byte
	var all []byte
	var cps []Checkpoint
	for i := 0; i < 5; i++ {
		part := testData(int64(200+i), 300<<10)
		cps = append(cps, Checkpoint{In: int64(len(compressed)) * 8, Out: int64(len(all)), Member: true})
		compressed = append(compressed, gzipped(t, gzip.DefaultCompression, part)...)
		all = append(all, part...)
	}
	built, _ := build(t, compressed, 1<<30) // for the sizes and hashes only
	idx := &Index{CompressedSize: built.CompressedSize, UncompressedSize: built.UncompressedSize,
		Chunk: built.Chunk, ChunkHashes: built.ChunkHashes, Checkpoints: cps}
	for _, off := range []int64{0, 300 << 10, 300<<10 - 10, 2*300<<10 + 12345, int64(len(all)) - 5000} {
		p := make([]byte, 5000)
		if err := idx.ReadAt(context.Background(), fetcher(compressed, nil), p, off); err != nil {
			t.Fatalf("ReadAt(%d): %v", off, err)
		}
		if !bytes.Equal(p, all[off:off+5000]) {
			t.Fatalf("ReadAt(%d) wrong from a member checkpoint", off)
		}
	}
}

func TestReaderDecompressesEachSegmentOnce(t *testing.T) {
	data := testData(21, 4<<20)
	compressed := gzipped(t, gzip.DefaultCompression, data)
	idx, _ := build(t, compressed, 512<<10)
	calls := 0
	var mu sync.Mutex
	fetch := func(_ context.Context, off, n int64) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return compressed[off : off+n], nil
	}
	r := NewReader(idx, fetch, 64<<20)
	// Many small reads inside the first megabyte, some of them at once.
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * 16 << 10
			p := make([]byte, 1000)
			if err := r.ReadAt(context.Background(), p, off); err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(p, data[off:off+1000]) {
				t.Errorf("wrong bytes at %d", off)
			}
		}(i)
	}
	wg.Wait()
	if calls > 3 {
		t.Errorf("%d fetches for reads inside two segments", calls)
	}
	// A range across every segment.
	p := make([]byte, len(data))
	if err := r.ReadAt(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, data) {
		t.Fatal("a read across all segments is wrong")
	}
}

func TestReaderStaysInBudget(t *testing.T) {
	data := testData(22, 4<<20)
	compressed := gzipped(t, gzip.DefaultCompression, data)
	idx, _ := build(t, compressed, 256<<10)
	r := NewReader(idx, fetcher(compressed, nil), 1<<20)
	p := make([]byte, len(data))
	if err := r.ReadAt(context.Background(), p, 0); err != nil {
		t.Fatal(err)
	}
	if r.used > 1<<20+idx.Checkpoints[1].Out*2 {
		t.Errorf("holding %d bytes with a 1 MiB budget", r.used)
	}
}
