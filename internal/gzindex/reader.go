package gzindex

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// Reader serves ranges of a stream's output. A segment - the output between
// two checkpoints - is decompressed once and kept, so the small files that
// sit next to each other in a layer cost one fetch and one decompression
// between them, however many reads they arrive in.
type Reader struct {
	idx   *Index
	fetch Fetch

	mu       sync.Mutex
	cached   map[int][]byte
	lru      []int // oldest first
	used     int64
	budget   int64
	inflight map[int]*segmentCall
}

type segmentCall struct {
	done chan struct{}
	data []byte
	err  error
}

// NewReader keeps up to budget bytes of decompressed segments.
func NewReader(idx *Index, fetch Fetch, budget int64) *Reader {
	return &Reader{idx: idx, fetch: fetch, budget: budget,
		cached: map[int][]byte{}, inflight: map[int]*segmentCall{}}
}

// ReadAt fills p with output from off.
func (r *Reader) ReadAt(ctx context.Context, p []byte, off int64) error {
	cps := r.idx.Checkpoints
	for len(p) > 0 {
		i := sort.Search(len(cps), func(i int) bool { return cps[i].Out > off }) - 1
		seg, err := r.segment(ctx, i)
		if err != nil {
			return err
		}
		n := copy(p, seg[off-cps[i].Out:])
		p, off = p[n:], off+int64(n)
	}
	return nil
}

// segment returns the output of segment i, decompressing it at most once
// however many readers want it at the same time.
func (r *Reader) segment(ctx context.Context, i int) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		data, err := r.segmentOnce(ctx, i)
		// The read this one waited on may have been cancelled, or timed
		// out, while this caller still has time: try again, a few times.
		// A fetch that times out on its own also reads as a deadline, so
		// the tries are bounded.
		if err != nil && attempt < 2 && ctx.Err() == nil &&
			(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			continue
		}
		return data, err
	}
}

func (r *Reader) segmentOnce(ctx context.Context, i int) ([]byte, error) {
	r.mu.Lock()
	if data, ok := r.cached[i]; ok {
		r.touch(i)
		r.mu.Unlock()
		return data, nil
	}
	if call, ok := r.inflight[i]; ok {
		r.mu.Unlock()
		select {
		case <-call.done:
			return call.data, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &segmentCall{done: make(chan struct{})}
	r.inflight[i] = call
	r.mu.Unlock()

	start := r.idx.Checkpoints[i].Out
	end := r.idx.UncompressedSize
	if i+1 < len(r.idx.Checkpoints) {
		end = r.idx.Checkpoints[i+1].Out
	}
	data := make([]byte, end-start)
	err := r.idx.ReadAt(ctx, r.fetch, data, start)
	if err != nil {
		data = nil
	}

	r.mu.Lock()
	delete(r.inflight, i)
	if err == nil {
		r.cached[i] = data
		r.used += int64(len(data))
		r.lru = append(r.lru, i)
		for r.used > r.budget && len(r.lru) > 1 {
			old := r.lru[0]
			r.lru = r.lru[1:]
			r.used -= int64(len(r.cached[old]))
			delete(r.cached, old)
		}
	}
	r.mu.Unlock()
	call.data, call.err = data, err
	close(call.done)
	return data, err
}

func (r *Reader) touch(i int) {
	for k, v := range r.lru {
		if v == i {
			r.lru = append(append(r.lru[:k:k], r.lru[k+1:]...), i)
			return
		}
	}
}
