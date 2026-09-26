package core

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/profile"
	"github.com/andreygrehov/range/internal/rangetest"
)

func TestReadConfigRejectsBadValues(t *testing.T) {
	t.Setenv("RANGE_BLOCK_SIZE", "1KiB") // below the 4KiB minimum
	if _, err := ReadConfig(); err == nil {
		t.Fatal("expected an error for a too-small block size")
	}
	t.Setenv("RANGE_BLOCK_SIZE", "1MiB")
	t.Setenv("RANGE_PREFETCH", "maybe")
	if _, err := ReadConfig(); err == nil {
		t.Fatal("expected an error for an invalid prefetch value")
	}
	t.Setenv("RANGE_PREFETCH", "off")
	c, err := ReadConfig()
	if err != nil {
		t.Fatalf("readConfig: %v", err)
	}
	if c.Prefetch {
		t.Fatal("prefetch should be off")
	}
	if c.BlockSize != 1<<20 {
		t.Fatalf("blockSize = %d, want %d", c.BlockSize, 1<<20)
	}
}

// TestReadAtMatchesSource is the load-bearing test: for arbitrary offsets and
// lengths, ReadAt must return exactly the bytes the artifact holds.
func TestReadAtMatchesSource(t *testing.T) {
	blockSizes := []int64{4 << 10, 64 << 10, 1 << 20}
	for _, blockSize := range blockSizes {
		t.Run(bytesize.Format(blockSize), func(t *testing.T) {
			data := rangetest.Data(1<<20 + 12345)
			r, _ := newTestReader(t, data, func(c *Config) {
				c.BlockSize = blockSize
				c.MaxRangeSize = blockSize * 3
			})
			rng := rand.New(rand.NewSource(7))
			for i := 0; i < 250; i++ {
				offset := rng.Int63n(int64(len(data)))
				length := rng.Int63n(int64(len(data))-offset) + 1
				buf := make([]byte, length)
				n, err := r.ReadAt(buf, offset)
				if err != nil && !errors.Is(err, io.EOF) {
					t.Fatalf("ReadAt(%d,%d): %v", offset, length, err)
				}
				if int64(n) != length {
					t.Fatalf("ReadAt(%d,%d) read %d bytes", offset, length, n)
				}
				if got, want := buf[:n], data[offset:offset+length]; string(got) != string(want) {
					t.Fatalf("ReadAt(%d,%d) returned wrong bytes", offset, length)
				}
			}
		})
	}
}

func TestReadAtEOFSemantics(t *testing.T) {
	data := rangetest.Data(10000)
	r, _ := newTestReader(t, data, func(c *Config) { c.BlockSize = 4 << 10 })

	t.Run("read past end returns EOF", func(t *testing.T) {
		buf := make([]byte, 16)
		n, err := r.ReadAt(buf, int64(len(data)))
		if n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("got (%d, %v), want (0, EOF)", n, err)
		}
	})

	t.Run("read straddling end returns partial plus EOF", func(t *testing.T) {
		buf := make([]byte, 500)
		n, err := r.ReadAt(buf, int64(len(data))-100)
		if !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want EOF", err)
		}
		if n != 100 {
			t.Fatalf("n = %d, want 100", n)
		}
		if string(buf[:n]) != string(data[len(data)-100:]) {
			t.Fatal("tail bytes did not match")
		}
	})

	t.Run("exact tail read has no EOF", func(t *testing.T) {
		buf := make([]byte, 100)
		n, err := r.ReadAt(buf, int64(len(data))-100)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if n != 100 {
			t.Fatalf("n = %d, want 100", n)
		}
	})

	t.Run("negative offset is an error", func(t *testing.T) {
		if _, err := r.ReadAt(make([]byte, 4), -1); err == nil {
			t.Fatal("expected an error for a negative offset")
		}
	})
}

func TestCacheAvoidsRepeatRemoteRequests(t *testing.T) {
	data := rangetest.Data(512 << 10)
	r, counter := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })

	buf := make([]byte, 200<<10)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("cold read: %v", err)
	}
	coldReads, coldBytes := counter.Counts()
	if coldReads == 0 {
		t.Fatal("cold read made no remote requests")
	}
	counter.Reset()

	for i := 0; i < 5; i++ {
		if _, err := r.ReadAt(buf, 0); err != nil {
			t.Fatalf("warm read: %v", err)
		}
	}
	warmReads, _ := counter.Counts()
	if warmReads != 0 {
		t.Fatalf("warm reads issued %d remote requests, want 0", warmReads)
	}
	if coldBytes < int64(len(buf)) {
		t.Fatalf("cold read fetched %d bytes for a %d byte read", coldBytes, len(buf))
	}
}

func TestAdjacentBlocksCoalesceIntoOneRequest(t *testing.T) {
	data := rangetest.Data(512 << 10)
	r, counter := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.MaxRangeSize = 8 << 20
	})
	buf := make([]byte, 4*(64<<10))
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	reads, bytes := counter.Counts()
	if reads != 1 {
		t.Fatalf("four adjacent missing blocks produced %d requests, want 1", reads)
	}
	if bytes != int64(len(buf)) {
		t.Fatalf("fetched %d bytes, want %d", bytes, len(buf))
	}
	if string(buf) != string(data[:len(buf)]) {
		t.Fatal("coalesced read returned wrong bytes")
	}
}

func TestMaxRangeSplitsLargeReads(t *testing.T) {
	data := rangetest.Data(512 << 10)
	r, counter := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.MaxRangeSize = 128 << 10 // two blocks per request
	})
	buf := make([]byte, 4*(64<<10))
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if reads, _ := counter.Counts(); reads != 2 {
		t.Fatalf("got %d requests, want 2 (maxRange should cap each one)", reads)
	}
	if string(buf) != string(data[:len(buf)]) {
		t.Fatal("split read returned wrong bytes")
	}
}

func TestConcurrentReadersShareOneRequest(t *testing.T) {
	data := rangetest.Data(1 << 20)
	r, counter := newTestReader(t, data, func(c *Config) { c.BlockSize = 1 << 20 })
	counter.Delay = 50 * time.Millisecond

	const readers = 20
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			if _, err := r.ReadAt(buf, 0); err != nil {
				errs <- err
				return
			}
			if string(buf) != string(data[:4096]) {
				errs <- errors.New("wrong bytes")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read: %v", err)
	}
	if reads, _ := counter.Counts(); reads != 1 {
		t.Fatalf("%d concurrent readers produced %d remote requests, want 1", readers, reads)
	}
	if got := r.stats.Coalesced.Load(); got != readers-1 {
		t.Fatalf("coalesced = %d, want %d", got, readers-1)
	}
}

func TestConcurrentMultiBlockReadsDeduplicate(t *testing.T) {
	data := rangetest.Data(1 << 20)
	r, counter := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.MaxRangeSize = 8 << 20
	})
	counter.Delay = 40 * time.Millisecond

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 512<<10)
			if _, err := r.ReadAt(buf, 0); err != nil {
				t.Errorf("read: %v", err)
				return
			}
			if string(buf) != string(data[:512<<10]) {
				t.Error("wrong bytes from concurrent multi-block read")
			}
		}()
	}
	wg.Wait()
	reads, bytes := counter.Counts()
	if reads == 0 {
		t.Fatal("no remote requests were made")
	}
	// Racing readers may split the block run differently, so the request count
	// varies. What must hold is that no block was ever fetched twice.
	if want := int64(512 << 10); bytes != want {
		t.Fatalf("fetched %d bytes across %d requests, want exactly %d (blocks must not be fetched twice)",
			bytes, reads, want)
	}
}

func TestTransientFailuresAreRetried(t *testing.T) {
	shortRetries(t)
	data := rangetest.Data(128 << 10)
	r, _ := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })
	flaky := &flakyBackend{inner: r.Backend, failures: 2}
	r.Backend = flaky

	buf := make([]byte, 1024)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("read should have survived two transient failures: %v", err)
	}
	if string(buf) != string(data[:1024]) {
		t.Fatal("wrong bytes after retry")
	}
	if flaky.attempts != 3 {
		t.Fatalf("made %d attempts, want 3 (one initial plus two retries)", flaky.attempts)
	}
	if got := r.stats.Retries.Load(); got != 2 {
		t.Fatalf("Retries = %d, want 2", got)
	}
}

func TestFetchFailurePropagatesToAllWaiters(t *testing.T) {
	shortRetries(t)
	data := rangetest.Data(256 << 10)
	r, counter := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })
	counter.FailWith = errors.New("object store is down")
	counter.Delay = 20 * time.Millisecond

	var wg sync.WaitGroup
	failures := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.ReadAt(make([]byte, 4096), 0); err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	if len(failures) != 10 {
		t.Fatalf("%d of 10 readers saw the failure, want 10", len(failures))
	}

	// A later successful read must still work: the failed block must not be
	// stuck in the inflight registry.
	counter.FailWith = nil
	buf := make([]byte, 4096)
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("read after recovery: %v", err)
	}
	if string(buf) != string(data[:4096]) {
		t.Fatal("wrong bytes after recovery")
	}
}

// TestHitAccountingIsNotDoubleCounted guards the metric the whole product is
// sold on: a cold multi-block read is all misses, and the repeat is all hits.
func TestHitAccountingIsNotDoubleCounted(t *testing.T) {
	data := rangetest.Data(512 << 10)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.MaxRangeSize = 8 << 20
	})
	buf := make([]byte, 4*(64<<10))
	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("cold read: %v", err)
	}
	if got := r.stats.Misses.Load(); got != 4 {
		t.Fatalf("cold read recorded %d misses, want 4", got)
	}
	if hits := r.stats.MemoryHits.Load() + r.stats.DiskHits.Load(); hits != 0 {
		t.Fatalf("cold read recorded %d cache hits, want 0", hits)
	}
	snapshot := r.Snapshot()
	if rate := snapshot.HitRate(); rate != 0 {
		t.Fatalf("cold hit rate = %.1f%%, want 0", rate)
	}

	if _, err := r.ReadAt(buf, 0); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if got := r.stats.Misses.Load(); got != 4 {
		t.Fatalf("warm read added misses: %d, want 4", got)
	}
	if hits := r.stats.MemoryHits.Load() + r.stats.DiskHits.Load(); hits != 4 {
		t.Fatalf("warm read recorded %d hits, want 4", hits)
	}
	if rate := r.Snapshot().HitRate(); rate != 50 {
		t.Fatalf("hit rate = %.1f%%, want 50", rate)
	}
}

func TestPrefetchDisabled(t *testing.T) {
	data := rangetest.Data(2 << 20)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.Prefetch = false
	})
	for block := int64(0); block < 6; block++ {
		if _, err := r.ReadAt(make([]byte, 1024), block*(64<<10)); err != nil {
			t.Fatalf("read block %d: %v", block, err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := r.stats.PrefetchIssued.Load(); got != 0 {
		t.Fatalf("prefetch is off but %d blocks were prefetched", got)
	}
}

func TestParseProfileMode(t *testing.T) {
	for _, valid := range []string{"off", "record", "auto", "AUTO", " record "} {
		if _, err := ParseProfileMode(valid); err != nil {
			t.Errorf("parseProfileMode(%q): %v", valid, err)
		}
	}
	if _, err := ParseProfileMode("sometimes"); err == nil {
		t.Error("expected an error for an unknown profile mode")
	}
}

func TestProfilePrefetchRespectsBudget(t *testing.T) {
	data := rangetest.Data(1 << 20)
	r, counter := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })
	p := profile.Profile{Ranges: []profile.BlockRange{{StartBlock: 0, Count: 16, Observations: 1}}}

	r.PrefetchProfile(context.Background(), p, 128<<10) // exactly two blocks
	waitFor(t, 2*time.Second, func() bool {
		_, bytes := counter.Counts()
		return bytes >= 128<<10
	})
	time.Sleep(100 * time.Millisecond)
	reads, bytes := counter.Counts()
	// Two contiguous blocks are one range request, not two: replaying a
	// profile block by block is what this is meant to avoid.
	if reads != 1 {
		t.Fatalf("prefetched in %d requests, want 1 coalesced request", reads)
	}
	if bytes != 128<<10 {
		t.Fatalf("prefetched %d bytes, want %d", bytes, 128<<10)
	}
	if r.stats.PrefetchBytes.Load() != 128<<10 {
		t.Fatalf("PrefetchBytes = %d", r.stats.PrefetchBytes.Load())
	}
	if r.stats.DemandBytes.Load() != 0 {
		t.Fatalf("DemandBytes = %d, want 0", r.stats.DemandBytes.Load())
	}
}

// TestProfilePrefetchYieldsToDemand covers the scheduler rule: speculative
// work waits while the application is blocked on a read.
func TestProfilePrefetchYieldsToDemand(t *testing.T) {
	data := rangetest.Data(1 << 20)
	r, counter := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })

	r.demandInFlight.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.PrefetchProfile(context.Background(), profile.Profile{
			Ranges: []profile.BlockRange{{StartBlock: 0, Count: 4, Observations: 1}},
		}, 0)
	}()

	time.Sleep(150 * time.Millisecond)
	if reads, _ := counter.Counts(); reads != 0 {
		t.Fatalf("prefetch issued %d requests while a demand read was in flight, want 0", reads)
	}
	r.demandInFlight.Add(-1)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("prefetch did not resume after demand drained")
	}
	// PrefetchProfile returns once it has issued the fetches, not once they have
	// landed, so wait for the requests rather than sampling immediately.
	waitFor(t, 3*time.Second, func() bool {
		reads, _ := counter.Counts()
		return reads > 0
	})
}

func TestProfilePrefetchSkipsCachedAndOutOfRangeBlocks(t *testing.T) {
	data := rangetest.Data(256 << 10)
	r, counter := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })
	if _, err := r.ReadAt(make([]byte, 1024), 0); err != nil {
		t.Fatal(err)
	}
	counter.Reset()
	r.PrefetchProfile(context.Background(), profile.Profile{Ranges: []profile.BlockRange{
		{StartBlock: 0, Count: 1, Observations: 1}, {StartBlock: 99, Count: 1, Observations: 1},
	}}, 0)
	time.Sleep(100 * time.Millisecond)
	if reads, _ := counter.Counts(); reads != 0 {
		t.Fatalf("prefetch made %d requests for cached or out-of-range blocks, want 0", reads)
	}
}

func TestPrefetchedBlockCountsAsHit(t *testing.T) {
	data := rangetest.Data(512 << 10)
	r, _ := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })
	r.PrefetchProfile(context.Background(), profile.Profile{
		Ranges: []profile.BlockRange{{StartBlock: 3, Count: 1, Observations: 1}},
	}, 0)
	waitFor(t, 2*time.Second, func() bool { return r.cached(3) })
	if _, err := r.ReadAt(make([]byte, 1024), 3*(64<<10)); err != nil {
		t.Fatalf("read prefetched block: %v", err)
	}
	if r.stats.PrefetchHits.Load() != 1 {
		t.Fatalf("PrefetchHits = %d, want 1", r.stats.PrefetchHits.Load())
	}
}

// TestRefetchDoesNotInflateWorkingSet guards the distinction the old
// "materialized" metric got wrong: bytes moved twice are one block of working set.
func TestRefetchDoesNotInflateWorkingSet(t *testing.T) {
	data := rangetest.Data(512 << 10)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.MemoryCache = 0 // force the block to be re-read from disk each time
	})
	for i := 0; i < 5; i++ {
		if _, err := r.ReadAt(make([]byte, 1024), 0); err != nil {
			t.Fatal(err)
		}
	}
	s := r.Snapshot()
	if s.WorkingSetBytes != 64<<10 {
		t.Fatalf("WorkingSetBytes = %d, want %d: one block read five times is still one block",
			s.WorkingSetBytes, 64<<10)
	}
}

func TestDemandBytesAreAccountedSeparately(t *testing.T) {
	data := rangetest.Data(512 << 10)
	r, _ := newTestReader(t, data, func(c *Config) {
		c.BlockSize = 64 << 10
		c.MaxRangeSize = 8 << 20
	})
	if _, err := r.ReadAt(make([]byte, 128<<10), 0); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot()
	if s.DemandBytes != 128<<10 {
		t.Fatalf("DemandBytes = %d, want %d", s.DemandBytes, 128<<10)
	}
	if s.PrefetchBytes != 0 {
		t.Fatalf("PrefetchBytes = %d, want 0", s.PrefetchBytes)
	}
	if s.WorkingSetBytes != 128<<10 {
		t.Fatalf("WorkingSetBytes = %d, want %d", s.WorkingSetBytes, 128<<10)
	}
	if s.Amplification() != 1 {
		t.Fatalf("amplification = %.2f, want 1.00 for an aligned full-block read", s.Amplification())
	}
}
