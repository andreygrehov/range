package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andreygrehov/range/internal/artifact"
	"github.com/andreygrehov/range/internal/cache"
	"github.com/andreygrehov/range/internal/hub"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/oci"
	"github.com/andreygrehov/range/internal/profile"
)

// Reader is an opened artifact: an io.ReaderAt over a remote object, pinned
// to the generation observed when it was opened.
type Reader struct {
	Backend   object.Backend
	Ident     object.Identity
	BlockSize int64
	maxRange  int64

	// Ceiling on one remote request; see config.requestTimeout.
	requestTimeout time.Duration

	// Set when the source is a compressed Range artifact rather than a raw
	// image, so the session can report compressed traffic as well as logical.
	artifact *artifact.Backend

	memory    *cache.Memory
	Disk      *cache.Disk
	inflight  *inflight
	stats     *counters
	base      Stats // counters carried over from earlier processes
	Trace     *TraceWriter
	ObjectDir string

	prefetchOn   bool
	prefetchMu   sync.Mutex
	lastRead     int64 // last block of the previous read, -1 before any
	streak       int   // reads in a row that carried on from the one before
	ahead        int64 // current sequential prefetch window, in blocks
	prefetched   map[int64]struct{}
	prefetchSlot chan struct{}
	prefetchWait sync.WaitGroup
	// Speculative work runs under a context the reader owns, so closing it
	// stops prefetches rather than leaving them to write into a cache
	// directory the caller believes is finished with.
	bgCtx    context.Context
	bgCancel context.CancelFunc

	// Demand reads outrank speculative ones: the profile prefetcher yields
	// while any demand fetch is in flight.
	demandInFlight atomic.Int64
	Recorder       *profile.Recorder

	// Set by "range shell" so every persisted snapshot names the session.
	SessionID   string
	SessionName string
	Workload    string
	ReadyIn     time.Duration

	closeOnce sync.Once
	closed    chan struct{}
}

// Open stats the object at uri, detects whether it is a Range artifact, and
// prepares its caches. Nothing but the header and index is read.
func Open(ctx context.Context, uri string, c Config) (*Reader, error) {
	// A Range artifact or raw image anywhere is read as it is. A Hugging Face
	// repository or a container image becomes an image here, with its files
	// read from where they already are.
	var b object.Backend
	var err error
	if name, ok := oci.ImageName(uri); ok {
		lazy := oci.NewLazy(name, oci.HostPlatform(), filepath.Join(c.CacheDir, "oci"))
		lazy.SetKeepLimit(c.DiskCache)
		b = lazy
	} else if hub.IsURI(uri) {
		b = hub.New()
	} else if b, err = object.Open(uri, c.s3Endpoint); err != nil {
		return nil, err
	}
	// A Range artifact announces itself in its first eight bytes. Everything
	// above this point works on logical offsets either way, so a compressed
	// artifact and a raw disk image are the same thing to the rest of Range.
	packed, err := artifact.Open(ctx, b, uri)
	if err != nil {
		return nil, err
	}
	if packed != nil {
		b = packed
	}
	info, err := b.Stat(ctx, uri)
	if err != nil {
		return nil, err
	}
	if info.Size <= 0 {
		return nil, fmt.Errorf("%s is empty", uri)
	}
	ident := object.Identity{
		URI: uri, Size: info.Size, ETag: info.ETag, VersionID: info.VersionID,
		LastModified: info.LastModified, BlockSize: c.BlockSize,
	}
	objectDir := filepath.Join(c.CacheDir, "objects", ident.Key())
	disk, err := cache.NewDisk(objectDir, c.DiskCache)
	if err != nil {
		return nil, fmt.Errorf("open cache: %w", err)
	}
	metadata, err := json.MarshalIndent(ident, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(objectDir, "metadata.json"), metadata, 0o644); err != nil {
		return nil, err
	}
	timeout := c.RequestTimeout
	if timeout <= 0 {
		timeout = DefaultConfig().RequestTimeout
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	return &Reader{
		artifact: packed,
		bgCtx:    bgCtx, bgCancel: bgCancel,
		Backend: b, Ident: ident, BlockSize: c.BlockSize, maxRange: c.MaxRangeSize,
		requestTimeout: timeout,
		memory:         cache.NewMemory(c.MemoryCache), Disk: disk, inflight: newInflight(),
		stats: &counters{}, base: previousStats(objectDir, ident), ObjectDir: objectDir,
		Recorder:   profile.NewRecorder(),
		prefetchOn: c.Prefetch, prefetched: make(map[int64]struct{}), lastRead: -1,
		prefetchSlot: make(chan struct{}, prefetchConcurrency), closed: make(chan struct{}),
	}, nil
}

// Size is the logical size of the artifact.
func (r *Reader) Size() int64 { return r.Ident.Size }

// BlockCount is the number of blocks covering Size.
func (r *Reader) BlockCount() int64 {
	return (r.Ident.Size + r.BlockSize - 1) / r.BlockSize
}

// blockLen is the block size except for the final, possibly short, block.
func (r *Reader) blockLen(block int64) int64 {
	remaining := r.Ident.Size - block*r.BlockSize
	if remaining < r.BlockSize {
		return remaining
	}
	return r.BlockSize
}

// ReadAt implements io.ReaderAt as a demand read.
func (r *Reader) ReadAt(p []byte, off int64) (int, error) {
	return r.readAt(context.Background(), p, off)
}

func (r *Reader) readAt(ctx context.Context, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("range: negative offset")
	}
	if off >= r.Ident.Size {
		return 0, io.EOF
	}
	var atEOF error
	want := int64(len(p))
	if off+want > r.Ident.Size {
		want = r.Ident.Size - off
		atEOF = io.EOF
		p = p[:want]
	}
	if want == 0 {
		return 0, atEOF
	}
	first := off / r.BlockSize
	last := (off + want - 1) / r.BlockSize
	// The working set is what the workload asked for, whatever layer served
	// it. Recording here rather than at the fetch makes a warm run observe the
	// same blocks as a cold one.
	for block := first; block <= last; block++ {
		r.Recorder.Record(block, r.blockLen(block))
	}
	fetched, err := r.prefill(ctx, first, last)
	if err != nil {
		return 0, err
	}
	copied := 0
	for block := first; block <= last; block++ {
		// Blocks prefill just pulled from the network are counted there; counting
		// them again on the way out of the cache would inflate the hit rate.
		_, prefetchedHere := fetched[block]
		data, err := r.block(ctx, block, !prefetchedHere)
		if err != nil {
			return copied, err
		}
		start := block * r.BlockSize
		from := int64(0)
		if off > start {
			from = off - start
		}
		to := int64(len(data))
		if end := off + want - start; end < to {
			to = end
		}
		if from >= to {
			continue
		}
		copied += copy(p[copied:], data[from:to])
	}
	r.stats.LogicalReads.Add(1)
	r.stats.RequestedBytes.Add(int64(copied))
	r.notePrefetch(first, last)
	if int64(copied) != want {
		return copied, fmt.Errorf("range: short read at offset %d, got %d of %d bytes", off, copied, want)
	}
	return copied, atEOF
}

func (r *Reader) cached(block int64) bool {
	return r.memory.Get(block) != nil || r.Disk.Has(block)
}

// prefill fetches the missing blocks of a read, merging adjacent misses into
// single remote requests up to maxRange. It reports which blocks it pulled from
// the network so the caller does not count them twice.
func (r *Reader) prefill(ctx context.Context, first, last int64) (map[int64]struct{}, error) {
	if last == first {
		return nil, nil // the single-block path in block() is enough
	}
	var fetched map[int64]struct{}
	runStart := int64(-1)
	var owned []*inflightEntry
	flush := func(runEnd int64) error {
		if runStart < 0 {
			return nil
		}
		start, end, entries := runStart, runEnd, owned
		runStart, owned = -1, nil
		if fetched == nil {
			fetched = make(map[int64]struct{})
		}
		for block := start; block <= end; block++ {
			fetched[block] = struct{}{}
		}
		r.stats.Misses.Add(end - start + 1)
		return r.fetchRun(ctx, start, end, entries, demandRead)
	}
	for block := first; block <= last; block++ {
		if r.cached(block) {
			if err := flush(block - 1); err != nil {
				return fetched, err
			}
			continue
		}
		entry, mine := r.inflight.claim(block)
		if !mine {
			if err := flush(block - 1); err != nil {
				return fetched, err
			}
			continue
		}
		if runStart < 0 {
			runStart = block
		}
		owned = append(owned, entry)
		if (block-runStart+1)*r.BlockSize >= r.maxRange {
			if err := flush(block); err != nil {
				return fetched, err
			}
		}
	}
	return fetched, flush(last)
}

// block returns one block, consulting memory, then disk, then the network.
// count is false when the caller has already accounted for this block.
func (r *Reader) block(ctx context.Context, id int64, count bool) ([]byte, error) {
	if data := r.memory.Get(id); data != nil {
		if count {
			r.stats.MemoryHits.Add(1)
			r.creditPrefetch(id)
		}
		return data, nil
	}
	if data, ok := r.Disk.Get(id, r.blockLen(id)); ok {
		r.memory.Put(id, data)
		if count {
			r.stats.DiskHits.Add(1)
			r.creditPrefetch(id)
		}
		return data, nil
	}
	entry, mine := r.inflight.claim(id)
	if !mine {
		if count {
			r.stats.Coalesced.Add(1)
		}
		return r.inflight.wait(ctx, entry)
	}
	if count {
		r.stats.Misses.Add(1)
	}
	if err := r.fetchRun(ctx, id, id, []*inflightEntry{entry}, demandRead); err != nil {
		return nil, err
	}
	return r.inflight.wait(ctx, entry)
}

// prefetchConcurrency bounds speculative requests in flight. Object-store
// throughput is a function of parallel connections, so a handful of slots
// leaves most of the available bandwidth unused.
const prefetchConcurrency = 16

// retryDelays bounds how hard a demand read tries. Three attempts, backing off
// in between; a read that is going to fail should fail quickly rather than hang
// a filesystem behind it.
var retryDelays = []time.Duration{100 * time.Millisecond, 250 * time.Millisecond}

func (r *Reader) readRangeWithRetry(ctx context.Context, offset, length int64) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		// Each attempt carries its own deadline. Without one a single stalled
		// connection holds an NBD read open forever, and the processes reading
		// the filesystem above it go into uninterruptible sleep.
		attemptCtx, cancel := context.WithTimeout(ctx, r.requestTimeout)
		data, err := r.Backend.ReadRange(attemptCtx, r.Ident.URI, offset, length, r.Ident.VersionID, r.Ident.ETag)
		cancel()
		if err == nil {
			return data, nil
		}
		if attempt >= len(retryDelays) || ctx.Err() != nil || !object.Retryable(err) {
			return nil, err
		}
		r.stats.Retries.Add(1)
		select {
		case <-time.After(retryDelays[attempt]):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// fetchRun issues one remote range request covering blocks [first,last] and
// resolves the inflight entries the caller claimed for them.
func (r *Reader) fetchRun(ctx context.Context, first, last int64, entries []*inflightEntry, class readClass) error {
	offset := first * r.BlockSize
	length := last*r.BlockSize + r.blockLen(last) - offset
	if class == demandRead {
		r.demandInFlight.Add(1)
		defer r.demandInFlight.Add(-1)
	}
	started := time.Now()
	data, err := r.readRangeWithRetry(ctx, offset, length)
	elapsed := time.Since(started)
	if err == nil {
		r.stats.RemoteRequests.Add(1)
		r.stats.RemoteBytes.Add(length)
		r.stats.RemoteNanos.Add(int64(elapsed))
		if class == demandRead {
			r.stats.DemandBytes.Add(length)
		} else {
			r.stats.PrefetchBytes.Add(length)
		}
		r.Trace.record(traceRecord{
			FirstBlock: first, LastBlock: last, Offset: offset, Length: length,
			Source: class.String(), LatencyMillis: elapsed.Milliseconds(),
		})
	}
	for index, block := 0, first; block <= last; block++ {
		if index >= len(entries) {
			break
		}
		entry := entries[index]
		index++
		if err != nil {
			r.inflight.resolve(block, entry, nil, err)
			continue
		}
		from := (block - first) * r.BlockSize
		// A slice of the coalesced buffer keeps the whole run alive for as long
		// as any one block is cached, so an 8 MiB read can pin 8 MiB while the
		// memory cache accounts for 1 MiB. Each block gets its own array.
		piece := append([]byte(nil), data[from:from+r.blockLen(block)]...)
		if putErr := r.Disk.Put(block, piece); putErr != nil {
			r.inflight.resolve(block, entry, nil, putErr)
			continue
		}
		r.memory.Put(block, piece)
		r.inflight.resolve(block, entry, piece, nil)
	}
	return err
}

// notePrefetch fetches ahead of a reader that walks forward. A read carries on
// from the one before when it starts at or before the block after the last one
// read and ends past it, however many blocks it spans: the kernel's readahead
// asks for more than one block at a time. Once prefetchWindow reads in a row
// have carried on, it fetches prefetchAhead blocks ahead, and doubles that
// window with every further one, up to prefetchMaxAhead. Blocks ahead go out
// in runs of up to maxRange, so a stream costs one request per run.
func (r *Reader) notePrefetch(first, last int64) {
	if !r.prefetchOn {
		return
	}
	r.prefetchMu.Lock()
	if last == r.lastRead {
		// Another piece of the same block is neither progress nor a break
		// in the pattern.
		r.prefetchMu.Unlock()
		return
	}
	if r.lastRead >= 0 && first <= r.lastRead+1 && last > r.lastRead {
		r.streak++
	} else {
		r.streak = 0
	}
	r.lastRead = last
	sequential := r.streak >= prefetchWindow-1
	switch {
	case !sequential:
		r.ahead = 0
	case r.ahead == 0:
		r.ahead = prefetchAhead
	default:
		r.ahead = min(2*r.ahead, prefetchMaxAhead)
	}
	ahead := r.ahead
	r.prefetchMu.Unlock()
	if ahead == 0 {
		return
	}

	maxRun := max(r.maxRange/r.BlockSize, 1)
	end := min(last+ahead, r.BlockCount()-1)
	for block := last + 1; block <= end; {
		if r.cached(block) {
			block++
			continue
		}
		select {
		case r.prefetchSlot <- struct{}{}:
		default:
			return // never let prefetch queue up behind demand reads
		}
		first := block
		var entries []*inflightEntry
		for block <= end && int64(len(entries)) < maxRun && !r.cached(block) {
			entry, mine := r.inflight.claim(block)
			if !mine {
				break
			}
			entries = append(entries, entry)
			block++
		}
		if len(entries) == 0 {
			<-r.prefetchSlot
			block++ // already being fetched by someone else
			continue
		}
		runLast := first + int64(len(entries)) - 1
		r.prefetchMu.Lock()
		for id := first; id <= runLast; id++ {
			r.prefetched[id] = struct{}{}
		}
		r.prefetchMu.Unlock()
		r.stats.PrefetchIssued.Add(int64(len(entries)))
		r.prefetchWait.Add(1)
		go func(first, last int64, entries []*inflightEntry) {
			defer r.prefetchWait.Done()
			defer func() { <-r.prefetchSlot }()
			ctx, cancel := context.WithTimeout(r.bgCtx, 60*time.Second)
			defer cancel()
			_ = r.fetchRun(ctx, first, last, entries, prefetchRead)
		}(first, runLast, entries)
	}
}

func (r *Reader) creditPrefetch(block int64) {
	r.prefetchMu.Lock()
	defer r.prefetchMu.Unlock()
	if _, ok := r.prefetched[block]; ok {
		delete(r.prefetched, block)
		r.stats.PrefetchHits.Add(1)
	}
}

// readClass separates reads the application is blocked on from speculative
// ones. Demand always outranks prefetch.
type readClass int

const (
	demandRead readClass = iota
	prefetchRead
)

func (c readClass) String() string {
	if c == prefetchRead {
		return "prefetch"
	}
	return "demand"
}

// PrefetchProfile replays a previous session's working set in the background.
// It never blocks startup, yields whenever a demand read is in flight, and
// stops once it has spent its budget.
func (r *Reader) PrefetchProfile(ctx context.Context, p profile.Profile, limit int64) {
	maxBlocks := r.maxRange / r.BlockSize
	if maxBlocks < 1 {
		maxBlocks = 1
	}
	// Runs in the profile split whenever their timing buckets differ, so a slow
	// session can leave a working set recorded as hundreds of one-block runs.
	// For fetching, only contiguity matters: spans are planned across run
	// boundaries, in the order the ranking gives, so priority is kept and the
	// requests are large.
	inProfile := make(map[int64]struct{}, p.BlockTotal())
	for _, run := range p.Ranges {
		for i := int64(0); i < run.Count; i++ {
			inProfile[run.StartBlock+i] = struct{}{}
		}
	}
	planned := make(map[int64]struct{}, len(inProfile))

	var spent int64
	runStart := int64(-1)
	var owned []*inflightEntry

	// flush issues everything claimed so far as a single range request. A span
	// breaks on a cached block, one another reader already claimed, or the
	// maxRange ceiling - the same rule the demand path uses.
	flush := func(runEnd int64) {
		if runStart < 0 {
			return
		}
		first, last, entries := runStart, runEnd, owned
		runStart, owned = -1, nil
		select {
		case r.prefetchSlot <- struct{}{}:
		case <-ctx.Done():
			for i, entry := range entries {
				r.inflight.resolve(first+int64(i), entry, nil, ctx.Err())
			}
			return
		}
		r.prefetchWait.Add(1)
		go func() {
			defer r.prefetchWait.Done()
			defer func() { <-r.prefetchSlot }()
			_ = r.fetchRun(ctx, first, last, entries, prefetchRead)
		}()
	}

	for _, head := range p.PrefetchOrder() {
		if _, done := planned[head]; done {
			continue
		}
		spanEnd := head
		for spanEnd-head+1 < maxBlocks {
			next := spanEnd + 1
			if _, ok := inProfile[next]; !ok {
				break
			}
			if _, done := planned[next]; done {
				break
			}
			spanEnd = next
		}
		for block := head; block <= spanEnd; block++ {
			planned[block] = struct{}{}
		}
		for block := head; block <= spanEnd; block++ {
			if ctx.Err() != nil {
				flush(block - 1)
				return
			}
			if limit > 0 && spent >= limit {
				flush(block - 1)
				return
			}
			if block >= r.BlockCount() || r.cached(block) {
				flush(block - 1)
				continue
			}
			// Demand always wins: give the span up rather than make a reader
			// queue behind speculative bytes.
			for r.demandInFlight.Load() > 0 {
				flush(block - 1)
				select {
				case <-time.After(2 * time.Millisecond):
				case <-ctx.Done():
					return
				}
			}
			entry, mine := r.inflight.claim(block)
			if !mine {
				flush(block - 1)
				continue
			}
			if runStart < 0 {
				runStart = block
			}
			owned = append(owned, entry)
			r.prefetchMu.Lock()
			r.prefetched[block] = struct{}{}
			r.prefetchMu.Unlock()
			r.stats.PrefetchIssued.Add(1)
			spent += r.blockLen(block)
		}
		flush(spanEnd)
	}
}

// verify re-checks that the remote object is still the artifact we opened.
func (r *Reader) verify(ctx context.Context) error {
	info, err := r.Backend.Stat(ctx, r.Ident.URI)
	if err != nil {
		return err
	}
	if !r.Ident.SameArtifact(info) {
		return object.ErrChanged
	}
	return nil
}

// Snapshot returns the statistics so far, including those of earlier
// processes on the same artifact.
func (r *Reader) Snapshot() Stats {
	return Stats{
		URI: r.Ident.URI, Size: r.Ident.Size, BlockSize: r.BlockSize,
		LogicalReads:       r.base.LogicalReads + r.stats.LogicalReads.Load(),
		RequestedBytes:     r.base.RequestedBytes + r.stats.RequestedBytes.Load(),
		RemoteRequests:     r.base.RemoteRequests + r.stats.RemoteRequests.Load(),
		RemoteBytes:        r.base.RemoteBytes + r.stats.RemoteBytes.Load(),
		RemoteNanos:        r.base.RemoteNanos + r.stats.RemoteNanos.Load(),
		MemoryHits:         r.base.MemoryHits + r.stats.MemoryHits.Load(),
		DiskHits:           r.base.DiskHits + r.stats.DiskHits.Load(),
		Misses:             r.base.Misses + r.stats.Misses.Load(),
		Coalesced:          r.base.Coalesced + r.stats.Coalesced.Load(),
		PrefetchIssued:     r.base.PrefetchIssued + r.stats.PrefetchIssued.Load(),
		PrefetchHits:       r.base.PrefetchHits + r.stats.PrefetchHits.Load(),
		Retries:            r.base.Retries + r.stats.Retries.Load(),
		DemandBytes:        r.base.DemandBytes + r.stats.DemandBytes.Load(),
		PrefetchBytes:      r.base.PrefetchBytes + r.stats.PrefetchBytes.Load(),
		CachedBytes:        r.Disk.Used(),
		UpdatedAt:          time.Now(),
		SessionID:          r.SessionID,
		SessionName:        r.SessionName,
		Workload:           r.Workload,
		ReadyNanos:         int64(r.ReadyIn),
		WorkingSetBytes:    r.Recorder.WorkingSet(),
		SessionRemoteBytes: r.stats.RemoteBytes.Load(),
		SessionCompressedBytes: func() int64 {
			if wire, ok := r.Backend.(object.WireCounter); ok {
				return wire.WireBytes()
			}
			return 0
		}(),
		SessionCompressedReqs: func() int64 {
			if wire, ok := r.Backend.(object.WireCounter); ok {
				return wire.WireRequests()
			}
			return 0
		}(),
		StoredSize: func() int64 {
			if r.artifact == nil {
				return 0
			}
			return r.artifact.Object.Size
		}(),
		SessionRequests: r.stats.RemoteRequests.Load(),
	}
}

// WriteStats persists Snapshot beside the cache.
func (r *Reader) WriteStats() error {
	data, err := json.MarshalIndent(r.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.ObjectDir, "stats.json"), data, 0o644)
}

// PublishStats keeps stats.json fresh so "range stats" can watch a running mount.
func (r *Reader) PublishStats(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_ = r.WriteStats()
		case <-r.closed:
			return
		}
	}
}

// Close stops background work, writes statistics and closes the trace.
func (r *Reader) Close() error {
	var err error
	r.closeOnce.Do(func() {
		close(r.closed)
		// Speculative fetches write into the cache directory; letting them
		// outlive the reader means writes landing after the caller believes
		// the session is over.
		r.bgCancel()
		r.prefetchWait.Wait()
		err = r.WriteStats()
		if traceErr := r.Trace.Close(); err == nil {
			err = traceErr
		}
	})
	return err
}
