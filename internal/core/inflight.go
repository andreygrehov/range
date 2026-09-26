package core

import (
	"context"
	"sync"
)

// Twenty goroutines asking for block 42 must produce one remote request.
type inflight struct {
	mu      sync.Mutex
	pending map[int64]*inflightEntry
}

type inflightEntry struct {
	done chan struct{}
	data []byte
	err  error
}

func newInflight() *inflight {
	return &inflight{pending: make(map[int64]*inflightEntry)}
}

// claim returns an entry and whether the caller now owns fetching it.
func (f *inflight) claim(block int64) (*inflightEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if entry, ok := f.pending[block]; ok {
		return entry, false
	}
	entry := &inflightEntry{done: make(chan struct{})}
	f.pending[block] = entry
	return entry, true
}

func (f *inflight) resolve(block int64, entry *inflightEntry, data []byte, err error) {
	f.mu.Lock()
	if f.pending[block] == entry {
		delete(f.pending, block)
	}
	f.mu.Unlock()
	entry.data, entry.err = data, err
	close(entry.done)
}

func (f *inflight) wait(ctx context.Context, entry *inflightEntry) ([]byte, error) {
	select {
	case <-entry.done:
		return entry.data, entry.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
