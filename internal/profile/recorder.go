package profile

import (
	"sync"
	"time"
)

// Recorder remembers which blocks a workload demanded, and how far into the
// session it first asked for each. It sits on the logical read path, so a warm
// run observes the same working set as a cold one.
type Recorder struct {
	mu    sync.Mutex
	start time.Time
	seen  map[int64]time.Duration
	bytes int64 // unique demand bytes: the working set
}

// NewRecorder returns a recorder with nothing observed.
func NewRecorder() *Recorder {
	return &Recorder{start: time.Now(), seen: make(map[int64]time.Duration)}
}

// Record notes a demand read of size bytes at block, once per block.
func (rec *Recorder) Record(block, size int64) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if _, ok := rec.seen[block]; ok {
		return
	}
	rec.seen[block] = time.Since(rec.start)
	rec.bytes += size
}

// WorkingSet is the number of distinct bytes the workload actually demanded.
func (rec *Recorder) WorkingSet() int64 {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.bytes
}

// Count is the number of distinct blocks observed.
func (rec *Recorder) Count() int64 {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return int64(len(rec.seen))
}

// Observations copies what this session saw, for merging into a profile.
func (rec *Recorder) Observations() map[int64]time.Duration {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make(map[int64]time.Duration, len(rec.seen))
	for block, at := range rec.seen {
		out[block] = at
	}
	return out
}
