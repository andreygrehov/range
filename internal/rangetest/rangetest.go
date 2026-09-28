// Package rangetest holds the test helpers shared across Range's packages:
// deterministic data, a backend that counts what crosses the "network", and
// stdout capture. It imports nothing of Range's but object, so any package's
// tests - core's included - can use it without an import cycle.
package rangetest

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/object"
)

// CountingBackend wraps a real backend and records every remote request, which
// is how most tests assert "we did not go to the network again".
type CountingBackend struct {
	Inner object.Backend
	Delay time.Duration

	mu        sync.Mutex
	readCalls int
	readBytes int64
	FailWith  error
}

// Stat passes through to the wrapped backend.
func (b *CountingBackend) Stat(ctx context.Context, uri string) (object.Info, error) {
	return b.Inner.Stat(ctx, uri)
}

// ReadRange counts the request, then fails with FailWith or waits Delay and
// passes it through.
func (b *CountingBackend) ReadRange(ctx context.Context, uri string, offset, length int64, version, etag string) ([]byte, error) {
	b.mu.Lock()
	b.readCalls++
	b.readBytes += length
	failure := b.FailWith
	b.mu.Unlock()
	if failure != nil {
		return nil, failure
	}
	if b.Delay > 0 {
		time.Sleep(b.Delay)
	}
	return b.Inner.ReadRange(ctx, uri, offset, length, version, etag)
}

// Counts reports the requests and bytes read so far.
func (b *CountingBackend) Counts() (reads int, bytes int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.readCalls, b.readBytes
}

// Reset zeroes the counters.
func (b *CountingBackend) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.readCalls, b.readBytes = 0, 0
}

// Data produces deterministic bytes that make off-by-one errors visible.
func Data(n int) []byte {
	data := make([]byte, n)
	rng := rand.New(rand.NewSource(int64(n)))
	rng.Read(data)
	for i := 0; i < n; i += 997 {
		data[i] = byte(i % 251)
	}
	return data
}

// ArtifactSource builds a file that exercises every case the artifact format
// has: data, a large run of zeroes, and a chunk repeated verbatim.
func ArtifactSource(t *testing.T, chunk int64) (string, []byte) {
	t.Helper()
	data := make([]byte, chunk*8)
	rng := rand.New(rand.NewSource(11))
	rng.Read(data[0:chunk])                  // chunk 0: incompressible
	copy(data[chunk:2*chunk], data[0:chunk]) // chunk 1: identical to chunk 0
	for i := int64(2 * chunk); i < 3*chunk; i++ {
		data[i] = byte('a' + i%7) // chunk 2: highly compressible
	}
	// chunks 3..5 stay zero
	copy(data[6*chunk:], []byte("tail marker"))
	path := filepath.Join(t.TempDir(), "src.img")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, data
}

// QuietStdout discards what a command prints for the rest of the test.
func QuietStdout(t *testing.T) {
	t.Helper()
	sink, err := os.CreateTemp(t.TempDir(), "stdout-*")
	if err != nil {
		t.Fatalf("create stdout sink: %v", err)
	}
	original := os.Stdout
	os.Stdout = sink
	t.Cleanup(func() {
		os.Stdout = original
		sink.Close()
	})
}

// CaptureStdout redirects stdout to a file and returns its contents. The file
// is not a character device, so anything that adapts to a terminal takes the
// non-terminal path.
func CaptureStdout(t *testing.T) (func() string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdout")
	sink, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = sink
	restore := func() {
		os.Stdout = original
		sink.Close()
	}
	t.Cleanup(restore)
	return func() string {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}, restore
}
