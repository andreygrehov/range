package core

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/artifact"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/profile"
	"github.com/andreygrehov/range/internal/rangetest"
)

func TestBlockMath(t *testing.T) {
	// 2.5 blocks: the final block is short, which is where off-by-ones live.
	data := rangetest.Data(64<<10*2 + 1000)
	r, _ := newTestReader(t, data, nil)

	if got, want := r.BlockCount(), int64(3); got != want {
		t.Fatalf("blockCount = %d, want %d", got, want)
	}
	if got, want := r.blockLen(0), int64(64<<10); got != want {
		t.Fatalf("blockLen(0) = %d, want %d", got, want)
	}
	if got, want := r.blockLen(2), int64(1000); got != want {
		t.Fatalf("blockLen(2) = %d, want %d", got, want)
	}
	if got, want := r.Size(), int64(len(data)); got != want {
		t.Fatalf("Size = %d, want %d", got, want)
	}
}

func TestCacheSurvivesReopen(t *testing.T) {
	data := rangetest.Data(256 << 10)
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, nil)

	first, err := Open(context.Background(), path, c)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	buf := make([]byte, 128<<10)
	if _, err := first.ReadAt(buf, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	first.Close()

	second, err := Open(context.Background(), path, c)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	counter := &rangetest.CountingBackend{Inner: second.Backend}
	second.Backend = counter
	if _, err := second.ReadAt(buf, 0); err != nil {
		t.Fatalf("read after reopen: %v", err)
	}
	if reads, _ := counter.Counts(); reads != 0 {
		t.Fatalf("reopened reader made %d remote requests, want 0 (disk cache should serve it)", reads)
	}
}

func TestRetriesAreBounded(t *testing.T) {
	shortRetries(t)
	data := rangetest.Data(128 << 10)
	r, _ := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })
	flaky := &flakyBackend{inner: r.Backend, failures: 100}
	r.Backend = flaky

	if _, err := r.ReadAt(make([]byte, 1024), 0); err == nil {
		t.Fatal("expected the read to fail once retries were exhausted")
	}
	if want := len(retryDelays) + 1; flaky.attempts != want {
		t.Fatalf("made %d attempts, want %d; retries must not be unbounded", flaky.attempts, want)
	}
}

// TestStatsAccumulateAcrossProcesses covers the CLI case: each command is a new
// process, so downloaded bytes must carry forward rather than reset while the
// cache on disk keeps growing.
func TestStatsAccumulateAcrossProcesses(t *testing.T) {
	data := rangetest.Data(512 << 10)
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, func(c *Config) { c.BlockSize = 64 << 10 })

	var lastSnapshot Stats
	for session := 0; session < 3; session++ {
		r, err := Open(context.Background(), path, c)
		if err != nil {
			t.Fatalf("open %d: %v", session, err)
		}
		// Each session reads a different block, so each downloads more.
		offset := int64(session) * (64 << 10)
		if _, err := r.ReadAt(make([]byte, 1024), offset); err != nil {
			t.Fatalf("read %d: %v", session, err)
		}
		lastSnapshot = r.Snapshot()
		if err := r.Close(); err != nil {
			t.Fatalf("close %d: %v", session, err)
		}
	}

	if want := int64(3); lastSnapshot.RemoteRequests != want {
		t.Fatalf("RemoteRequests = %d after three sessions, want %d", lastSnapshot.RemoteRequests, want)
	}
	if want := int64(3 * (64 << 10)); lastSnapshot.RemoteBytes != want {
		t.Fatalf("RemoteBytes = %d, want %d", lastSnapshot.RemoteBytes, want)
	}
	if lastSnapshot.RemoteBytes < lastSnapshot.CachedBytes {
		t.Fatalf("downloaded %d bytes but reports %d cached; downloaded must never be less",
			lastSnapshot.RemoteBytes, lastSnapshot.CachedBytes)
	}
	if lastSnapshot.LogicalReads != 3 {
		t.Fatalf("LogicalReads = %d, want 3", lastSnapshot.LogicalReads)
	}
}

func TestStatsDoNotCarryAcrossDifferentArtifacts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(path, rangetest.Data(128<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, func(c *Config) { c.BlockSize = 64 << 10 })
	r, err := Open(context.Background(), path, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadAt(make([]byte, 1024), 0); err != nil {
		t.Fatal(err)
	}
	objectDir := r.ObjectDir
	r.Close()

	// A stats file describing some other artifact must be ignored.
	other := Stats{URI: "s3://somewhere/else", Size: 999, RemoteBytes: 12345}
	blob, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(objectDir, "stats.json"), blob, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := Open(context.Background(), path, c)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.Snapshot().RemoteBytes; got != 0 {
		t.Fatalf("RemoteBytes = %d, want 0 (stats for a different artifact must not carry over)", got)
	}
}

func TestVerifyDetectsChangedObject(t *testing.T) {
	data := rangetest.Data(64 << 10)
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, nil)
	r, err := Open(context.Background(), path, c)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	if err := r.verify(context.Background()); err != nil {
		t.Fatalf("verify on an unchanged object: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, rangetest.Data(128<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.verify(context.Background()); !errors.Is(err, object.ErrChanged) {
		t.Fatalf("verify after mutation = %v, want errObjectChanged", err)
	}
}

func TestHTTPBackendRangeRequests(t *testing.T) {
	data := rangetest.Data(300 << 10)
	var gotRange string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("ETag", `"test-etag"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		gotRange = r.Header.Get("Range")
		var first, last int64
		fmt.Sscanf(gotRange, "bytes=%d-%d", &first, &last)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[first : last+1])
	}))
	defer server.Close()

	c := testConfig(t, func(c *Config) { c.BlockSize = 64 << 10 })
	r, err := Open(context.Background(), server.URL+"/artifact.bin", c)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	if r.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", r.Size(), len(data))
	}
	if r.Ident.ETag != "test-etag" {
		t.Fatalf("ETag = %q, want test-etag", r.Ident.ETag)
	}
	buf := make([]byte, 5000)
	if _, err := r.ReadAt(buf, 70000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != string(data[70000:75000]) {
		t.Fatal("http backend returned wrong bytes")
	}
	// Offset 70000 with a 64KiB block lands in block 1: bytes 65536-131071.
	if gotRange != "bytes=65536-131071" {
		t.Fatalf("Range header = %q, want bytes=65536-131071", gotRange)
	}
}

func TestHTTPBackendRejectsServerWithoutRangeSupport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	c := testConfig(t, nil)
	if _, err := Open(context.Background(), server.URL+"/x", c); err == nil {
		t.Fatal("expected an error when the server does not advertise range support")
	}
}

func TestS3BackendAgainstFakeEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	data := rangetest.Data(400 << 10)
	const version = "v-12345"
	var sawVersion string
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("ETag", `"s3-etag"`)
		w.Header().Set("x-amz-version-id", version)
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		if got := r.URL.Query().Get("versionId"); got != "" {
			sawVersion = got
		}
		var first, last int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &first, &last)
		if last >= int64(len(data)) {
			last = int64(len(data)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", first, last, len(data)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(data[first : last+1])
	}))
	defer server.Close()

	c := testConfig(t, func(c *Config) {
		c.BlockSize = 64 << 10
		c.s3Endpoint = server.URL
	})
	r, err := Open(context.Background(), "s3://demo/linux.img", c)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()

	if r.Size() != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", r.Size(), len(data))
	}
	if r.Ident.VersionID != version {
		t.Fatalf("VersionID = %q, want %q", r.Ident.VersionID, version)
	}
	buf := make([]byte, 8192)
	if _, err := r.ReadAt(buf, 100000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != string(data[100000:108192]) {
		t.Fatal("s3 backend returned wrong bytes")
	}
	if sawVersion != version {
		t.Fatalf("reads did not pin the object version: got %q", sawVersion)
	}
	for _, path := range paths {
		if !strings.Contains(path, "demo/linux.img") {
			t.Fatalf("unexpected request path %q", path)
		}
	}
}

func TestFileBackendHandlesFileURI(t *testing.T) {
	data := rangetest.Data(8192)
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c := testConfig(t, func(c *Config) { c.BlockSize = 4 << 10 })
	r, err := Open(context.Background(), "file://"+path, c)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	buf := make([]byte, 100)
	if _, err := r.ReadAt(buf, 5000); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != string(data[5000:5100]) {
		t.Fatal("file backend returned wrong bytes")
	}
}

// TestWorkingSetIsIndependentOfCacheState is the property that makes profiles
// trustworthy: a warm run must observe the same working set as a cold one.
func TestWorkingSetIsIndependentOfCacheState(t *testing.T) {
	data := rangetest.Data(1 << 20)
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := t.TempDir()
	tune := func(c *Config) {
		c.BlockSize = 64 << 10
		c.CacheDir = cacheDir
		c.Prefetch = false
	}
	workload := func() []int64 {
		r, err := Open(context.Background(), path, testConfig(t, tune))
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		for _, offset := range []int64{0, 300000, 700000} {
			if _, err := r.ReadAt(make([]byte, 4096), offset); err != nil {
				t.Fatal(err)
			}
		}
		blocks := make([]int64, 0)
		for block := range r.Recorder.Observations() {
			blocks = append(blocks, block)
		}
		sort.Slice(blocks, func(i, j int) bool { return blocks[i] < blocks[j] })
		if r.Recorder.WorkingSet() == 0 {
			t.Fatal("working set was not measured")
		}
		return blocks
	}
	cold := workload() // populates the shared disk cache
	warm := workload() // served entirely from it
	if fmt.Sprint(cold) != fmt.Sprint(warm) {
		t.Fatalf("cold working set %v != warm working set %v; recording must not depend on cache state", cold, warm)
	}
}

// TestProfileLearningCycle is the v0.2 value claim in miniature: a session
// records what it touched, and the next one replays it ahead of demand.
func TestProfileLearningCycle(t *testing.T) {
	data := rangetest.Data(2 << 20)
	path := filepath.Join(t.TempDir(), "dev.img")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	profileDir := t.TempDir()
	tune := func(c *Config) {
		c.BlockSize = 64 << 10
		c.Prefetch = false
	}

	first, err := Open(context.Background(), path, testConfig(t, tune))
	if err != nil {
		t.Fatal(err)
	}
	touched := []int64{0, 1, 2, 17, 18, 30}
	for _, block := range touched {
		if _, err := first.ReadAt(make([]byte, 1024), block*(64<<10)); err != nil {
			t.Fatal(err)
		}
	}
	learned, err := profile.Save(profileDir, first.Ident, first.BlockSize, "go-test",
		first.Recorder.Observations(), time.Now())
	if err != nil {
		t.Fatalf("saveProfile: %v", err)
	}
	if learned.BlockTotal() != int64(len(touched)) {
		t.Fatalf("profile has %d blocks, want %d", learned.BlockTotal(), len(touched))
	}
	// How the recorder splits runs depends on session timing, so what matters
	// is that the runs cover exactly the blocks the workload touched.
	covered := map[int64]bool{}
	for _, run := range learned.Ranges {
		for i := int64(0); i < run.Count; i++ {
			covered[run.StartBlock+i] = true
		}
	}
	if len(covered) != len(touched) {
		t.Fatalf("ranges cover %d blocks, want %d: %+v", len(covered), len(touched), learned.Ranges)
	}
	for _, block := range touched {
		if !covered[block] {
			t.Fatalf("profile does not cover block %d: %+v", block, learned.Ranges)
		}
	}
	first.Close()

	second, err := Open(context.Background(), path, testConfig(t, tune)) // fresh byte cache
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	replay, ok := profile.Load(profileDir, second.Ident, second.BlockSize, "go-test")
	if !ok {
		t.Fatal("the stored profile did not load")
	}
	counter := &rangetest.CountingBackend{Inner: second.Backend}
	second.Backend = counter
	second.PrefetchProfile(context.Background(), replay, 0)
	waitFor(t, 5*time.Second, func() bool {
		for _, block := range touched {
			if !second.cached(block) {
				return false
			}
		}
		return true
	})
	// Blocks 0-2, 17-18 and 30 are three contiguous spans, so replay must take
	// three requests however the recorder happened to split the runs.
	reads, fetched := counter.Counts()
	if reads != 3 {
		t.Fatalf("prefetched in %d requests, want 3 (one per contiguous span)", reads)
	}
	if want := int64(len(touched)) * second.BlockSize; fetched != want {
		t.Fatalf("prefetched %d bytes, want %d", fetched, want)
	}
	counter.Reset()

	for _, block := range touched {
		buf := make([]byte, 1024)
		if _, err := second.ReadAt(buf, block*(64<<10)); err != nil {
			t.Fatal(err)
		}
		offset := block * (64 << 10)
		if string(buf) != string(data[offset:offset+1024]) {
			t.Fatalf("block %d returned wrong bytes after prefetch", block)
		}
	}
	if reads, _ := counter.Counts(); reads != 0 {
		t.Fatalf("the profiled session made %d demand requests, want 0", reads)
	}
	s := second.Snapshot()
	if s.DemandBytes != 0 {
		t.Fatalf("DemandBytes = %d, want 0", s.DemandBytes)
	}
	if s.PrefetchHits != int64(len(touched)) {
		t.Fatalf("PrefetchHits = %d, want %d", s.PrefetchHits, len(touched))
	}
}

func TestReadClassNames(t *testing.T) {
	if demandRead.String() != "demand" || prefetchRead.String() != "prefetch" {
		t.Fatal("read classes must be named in traces")
	}
}

func TestOpenRejectsCorruptArtifacts(t *testing.T) {
	source, _ := rangetest.ArtifactSource(t, 64<<10)
	target := source + ".range"
	if _, err := artifact.Pack(source, target, 64<<10); err != nil {
		t.Fatal(err)
	}
	valid, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	header, err := artifact.ParseHeader(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"truncated header", func(b []byte) []byte { return b[:len(artifact.Magic)] }},
		{"unsupported version", func(b []byte) []byte {
			binary.LittleEndian.PutUint32(b[8:], 99)
			return b
		}},
		{"invalid header", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[16:], 0)
			return b
		}},
		{"corrupt index", func(b []byte) []byte {
			b[header.IndexOffset] ^= 0xff
			return b
		}},
		{"truncated index", func(b []byte) []byte { return b[:len(b)-1] }},
		{"index count mismatch", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[48:], uint64(header.ChunkCount+1))
			binary.LittleEndian.PutUint64(b[24:], uint64((header.ChunkCount+1)*header.ChunkSize))
			return b
		}},
		{"chunk count disagrees with logical size", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[24:], uint64(header.LogicalSize+10*header.ChunkSize))
			return b
		}},
		{"chunk size of a terabyte", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[16:], 1<<40)
			binary.LittleEndian.PutUint64(b[48:], 1)
			return b
		}},
		{"billions of chunks", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[24:], uint64(header.ChunkSize)<<32)
			binary.LittleEndian.PutUint64(b[48:], 1<<32)
			return b
		}},
		{"index inside the header", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[32:], 10)
			return b
		}},
		{"index past the end", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[32:], uint64(len(b)+1))
			return b
		}},
		{"index length of an exabyte", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[40:], 1<<60)
			return b
		}},
		{"index offset and length overflow", func(b []byte) []byte {
			binary.LittleEndian.PutUint64(b[32:], 1<<62)
			binary.LittleEndian.PutUint64(b[40:], 1<<62)
			return b
		}},
		{"index that decompresses into a bomb", func(b []byte) []byte {
			return withIndex(t, b, header, make([]byte, 64<<20))
		}},
		{"chunk with a negative length", func(b []byte) []byte {
			return withEntry(t, b, header, func(e *artifact.Entry) { e.Length = -5 })
		}},
		{"chunk longer than a chunk", func(b []byte) []byte {
			return withEntry(t, b, header, func(e *artifact.Entry) { e.Length = int32(header.ChunkSize + 1) })
		}},
		{"chunk inside the header", func(b []byte) []byte {
			return withEntry(t, b, header, func(e *artifact.Entry) { e.Offset = 8 })
		}},
		{"chunk overlapping the index", func(b []byte) []byte {
			return withEntry(t, b, header, func(e *artifact.Entry) { e.Offset = header.IndexOffset - 1 })
		}},
		{"chunk with unknown flags", func(b []byte) []byte {
			return withEntry(t, b, header, func(e *artifact.Entry) { e.Flags |= 1 << 7 })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "broken.range")
			if err := os.WriteFile(path, tc.mutate(bytes.Clone(valid)), 0o644); err != nil {
				t.Fatal(err)
			}
			r, err := Open(context.Background(), path, testConfig(t, nil))
			if r != nil {
				r.Close()
			}
			if err == nil {
				t.Fatal("corrupt Range artifact was accepted as a raw image")
			}
			if !strings.Contains(err.Error(), "range artifact") {
				t.Fatalf("error should identify the artifact failure: %v", err)
			}
		})
	}
}

// A raw disk image must keep working: the probe has to leave it alone.
func TestOpenLeavesRawImagesAlone(t *testing.T) {
	for _, size := range []int{5, 8, 32, 256 << 10} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "raw.img")
			want := rangetest.Data(size)
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			if packed, err := artifact.Open(context.Background(), object.FileBackend{}, path); err != nil || packed != nil {
				t.Fatalf("raw image probe = %v, %v", packed, err)
			}
			r, err := Open(context.Background(), path, testConfig(t, nil))
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			got := make([]byte, size)
			if _, err := r.ReadAt(got, 0); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("raw image read: %v, bytes match: %v", err, bytes.Equal(got, want))
			}
		})
	}
}

// The whole point: everything above ReadAt sees the same logical image whether
// the bytes underneath are raw or compressed.
func TestOpenReadsCompressedArtifactTransparently(t *testing.T) {
	const chunk = 64 << 10
	source, want := rangetest.ArtifactSource(t, chunk)
	target := source + ".range"
	if _, err := artifact.Pack(source, target, chunk); err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), target, testConfig(t, func(c *Config) {
		c.BlockSize = chunk
	}))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close()
	if r.Size() != int64(len(want)) {
		t.Fatalf("Size = %d, want %d", r.Size(), len(want))
	}
	got := make([]byte, 5000)
	if _, err := r.ReadAt(got, int64(chunk)*2+17); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want[chunk*2+17:chunk*2+17+5000]) {
		t.Fatal("bytes read through the reader do not match the source")
	}
	if r.artifact == nil {
		t.Fatal("the reader did not record that its source is an artifact")
	}
	if r.artifact.CompressedBytes.Load() <= 0 {
		t.Error("compressed traffic was not accounted")
	}
}
