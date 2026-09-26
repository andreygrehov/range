package core

import (
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/rangetest"
)

func TestSequentialPrefetch(t *testing.T) {
	data := rangetest.Data(2 << 20)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.Prefetch = true
	})
	// Walk forward one block at a time; after prefetchWindow reads the runtime
	// should start pulling blocks ahead of us.
	for block := int64(0); block < prefetchWindow; block++ {
		if _, err := r.ReadAt(make([]byte, 1024), block*(64<<10)); err != nil {
			t.Fatalf("read block %d: %v", block, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r.stats.PrefetchIssued.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	issued := r.stats.PrefetchIssued.Load()
	if issued == 0 {
		t.Fatal("sequential reads did not trigger any prefetch")
	}
	if issued > prefetchAhead {
		t.Fatalf("prefetched %d blocks, want at most %d", issued, prefetchAhead)
	}

	// Wait for the prefetched block to land, then read it: it should be a hit.
	next := int64(prefetchWindow)
	for time.Now().Before(deadline) && !r.cached(next) {
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := r.ReadAt(make([]byte, 1024), next*(64<<10)); err != nil {
		t.Fatalf("read prefetched block: %v", err)
	}
	if r.stats.PrefetchHits.Load() == 0 {
		t.Fatal("reading a prefetched block was not credited as a prefetch hit")
	}
}
