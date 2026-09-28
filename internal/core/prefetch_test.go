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

// The kernel reads a file in pieces smaller than a block. A reader that walks
// forward in such pieces is sequential, and the window grows as it keeps going.
func TestSequentialPrefetchInSubBlockPieces(t *testing.T) {
	const block = 64 << 10
	data := rangetest.Data(4 << 20)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = block
		c.MaxRangeSize = 4 * block
		c.Prefetch = true
	})
	piece := make([]byte, block/4)
	for off := int64(0); off < 12*block; off += int64(len(piece)) {
		if _, err := r.ReadAt(piece, off); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
	}
	r.prefetchWait.Wait()
	if issued := r.stats.PrefetchIssued.Load(); issued <= prefetchAhead {
		t.Fatalf("prefetched %d blocks walking 12 blocks in quarter-block pieces, want the window to grow past %d",
			issued, prefetchAhead)
	}
	r.prefetchMu.Lock()
	ahead := r.ahead
	r.prefetchMu.Unlock()
	if ahead <= prefetchAhead || ahead > prefetchMaxAhead {
		t.Errorf("window %d blocks, want it grown past %d and at most %d", ahead, prefetchAhead, prefetchMaxAhead)
	}
	// Runs, not single blocks: fewer prefetch requests than prefetched blocks.
	if reqs, issued := r.stats.RemoteRequests.Load(), r.stats.PrefetchIssued.Load(); reqs >= 12+issued {
		t.Errorf("%d requests for 12 demand blocks and %d prefetched: prefetch did not coalesce", reqs, issued)
	}
}

// With a large readahead the kernel asks for two blocks at a time. That is
// still a reader walking forward.
func TestSequentialPrefetchInMultiBlockReads(t *testing.T) {
	const block = 64 << 10
	data := rangetest.Data(4 << 20)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = block
		c.MaxRangeSize = 4 * block
		c.Prefetch = true
	})
	piece := make([]byte, 2*block)
	for off := int64(0); off < 24*block; off += int64(len(piece)) {
		if _, err := r.ReadAt(piece, off); err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
	}
	r.prefetchWait.Wait()
	if r.stats.PrefetchIssued.Load() == 0 {
		t.Fatal("reads of two blocks at a time never started a prefetch")
	}
}

// A jump backwards, or a read that skips ahead, is not a stream.
func TestRandomReadsDoNotPrefetch(t *testing.T) {
	const block = 64 << 10
	data := rangetest.Data(4 << 20)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = block
		c.Prefetch = true
	})
	for _, b := range []int64{10, 3, 40, 7, 22, 1, 50, 30} {
		if _, err := r.ReadAt(make([]byte, 100), b*block); err != nil {
			t.Fatal(err)
		}
	}
	r.prefetchWait.Wait()
	if n := r.stats.PrefetchIssued.Load(); n != 0 {
		t.Errorf("random reads prefetched %d blocks", n)
	}
}
