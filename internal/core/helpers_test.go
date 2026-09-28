package core

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/andreygrehov/range/internal/artifact"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/rangetest"
)

// waitFor polls until condition holds, which keeps prefetch tests from racing.
func waitFor(t *testing.T, limit time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}

func testConfig(t *testing.T, tune func(*Config)) Config {
	t.Helper()
	c := DefaultConfig()
	c.CacheDir = t.TempDir()
	c.BlockSize = 64 << 10
	c.MaxRangeSize = 8 << 20
	c.MemoryCache = 4 << 20
	c.DiskCache = 64 << 20
	c.Prefetch = false
	if tune != nil {
		tune(&c)
	}
	if c.MaxRangeSize < c.BlockSize {
		c.MaxRangeSize = c.BlockSize
	}
	return c
}

// newTestReader writes data to a temp file, opens it, and swaps in a counting
// backend so tests can see exactly what crossed the "network".
func newTestReader(t *testing.T, data []byte, tune func(*Config)) (*Reader, *rangetest.CountingBackend) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	c := testConfig(t, tune)
	r, err := Open(context.Background(), path, c)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	counter := &rangetest.CountingBackend{Inner: r.Backend}
	r.Backend = counter
	return r, counter
}

// shortRetries makes the bounded backoff fast enough to test.
func shortRetries(t *testing.T) {
	t.Helper()
	original := retryDelays
	retryDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { retryDelays = original })
}

// flakyBackend fails a fixed number of times before succeeding.
type flakyBackend struct {
	inner    object.Backend
	mu       sync.Mutex
	failures int
	attempts int
}

func (b *flakyBackend) Stat(ctx context.Context, uri string) (object.Info, error) {
	return b.inner.Stat(ctx, uri)
}

func (b *flakyBackend) ReadRange(ctx context.Context, uri string, offset, length int64, version, etag string) ([]byte, error) {
	b.mu.Lock()
	b.attempts++
	shouldFail := b.failures > 0
	if shouldFail {
		b.failures--
	}
	b.mu.Unlock()
	if shouldFail {
		return nil, errors.New("transient object store error")
	}
	return b.inner.ReadRange(ctx, uri, offset, length, version, etag)
}

// withIndex replaces the index of an artifact with arbitrary bytes, compressed
// the way the writer compresses a real one.
func withIndex(t *testing.T, b []byte, h artifact.Header, raw []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	packed := enc.EncodeAll(raw, nil)
	enc.Close()
	out := append(b[:h.IndexOffset:h.IndexOffset], packed...)
	binary.LittleEndian.PutUint64(out[40:], uint64(len(packed)))
	return out
}

// withEntry rewrites the first stored chunk's index entry.
func withEntry(t *testing.T, b []byte, h artifact.Header, change func(*artifact.Entry)) []byte {
	t.Helper()
	dec, _ := zstd.NewReader(nil)
	raw, err := dec.DecodeAll(b[h.IndexOffset:h.IndexOffset+h.IndexLength], nil)
	dec.Close()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := artifact.ParseIndex(raw)
	for i := range entries {
		if !entries[i].Zero() {
			change(&entries[i])
			break
		}
	}
	return withIndex(t, b, h, artifact.MarshalIndex(entries))
}
