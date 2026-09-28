package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/andreygrehov/range/internal/object"
)

type counters struct {
	LogicalReads   atomic.Int64
	RequestedBytes atomic.Int64
	RemoteRequests atomic.Int64
	RemoteBytes    atomic.Int64
	RemoteNanos    atomic.Int64
	MemoryHits     atomic.Int64
	DiskHits       atomic.Int64
	Misses         atomic.Int64
	Coalesced      atomic.Int64
	PrefetchIssued atomic.Int64
	PrefetchHits   atomic.Int64
	Retries        atomic.Int64
	DemandBytes    atomic.Int64
	PrefetchBytes  atomic.Int64
}

// Stats is the on-disk shape written next to an artifact's cached blocks so
// that "range stats" can report on a reader running in another process.
type Stats struct {
	URI            string    `json:"uri"`
	Size           int64     `json:"size"`
	BlockSize      int64     `json:"block_size"`
	LogicalReads   int64     `json:"logical_reads"`
	RequestedBytes int64     `json:"requested_bytes"`
	RemoteRequests int64     `json:"remote_requests"`
	RemoteBytes    int64     `json:"remote_bytes"`
	RemoteNanos    int64     `json:"remote_nanos"`
	MemoryHits     int64     `json:"memory_hits"`
	DiskHits       int64     `json:"disk_hits"`
	Misses         int64     `json:"misses"`
	Coalesced      int64     `json:"coalesced"`
	PrefetchIssued int64     `json:"prefetch_issued"`
	PrefetchHits   int64     `json:"prefetch_hits"`
	Retries        int64     `json:"retries"`
	DemandBytes    int64     `json:"demand_bytes"`
	PrefetchBytes  int64     `json:"prefetch_bytes"`
	CachedBytes    int64     `json:"cached_bytes"`
	UpdatedAt      time.Time `json:"updated_at"`

	// This session only, overwritten rather than accumulated. Working set is a
	// union, not a sum, so it cannot be carried forward across processes.
	SessionID          string `json:"session_id,omitempty"`
	SessionName        string `json:"session_name,omitempty"`
	Workload           string `json:"workload,omitempty"`
	ReadyNanos         int64  `json:"ready_nanos,omitempty"`
	WorkingSetBytes    int64  `json:"working_set_bytes"`
	SessionRemoteBytes int64  `json:"session_remote_bytes"`
	// Set only for a compressed artifact: the bytes actually pulled from
	// object storage, which is what the network and the bill see.
	SessionCompressedBytes int64 `json:"session_compressed_bytes,omitempty"`
	SessionCompressedReqs  int64 `json:"session_compressed_requests,omitempty"`
	StoredSize             int64 `json:"stored_size,omitempty"`
	SessionRequests        int64 `json:"session_requests"`
}

// HitRate is the share of block reads served from a cache.
func (s Stats) HitRate() float64 {
	lookups := s.MemoryHits + s.DiskHits + s.Misses
	if lookups == 0 {
		return 0
	}
	return float64(s.MemoryHits+s.DiskHits) / float64(lookups) * 100
}

// AverageRemoteLatency is the mean duration of a remote request.
func (s Stats) AverageRemoteLatency() time.Duration {
	if s.RemoteRequests == 0 {
		return 0
	}
	return time.Duration(s.RemoteNanos / s.RemoteRequests)
}

// WorkingSetRatio is the share of the artifact the workload actually needed:
// distinct demanded bytes over artifact size. Refetching a block does not make
// the working set larger.
func (s Stats) WorkingSetRatio() float64 {
	if s.Size == 0 {
		return 0
	}
	return float64(s.WorkingSetBytes) / float64(s.Size) * 100
}

// TransferRatio is the share of the artifact that crossed the network this
// session, including refetches and speculative prefetch. It can exceed the
// working-set ratio, and in a pathological case could exceed 100%.
func (s Stats) TransferRatio() float64 {
	if s.Size == 0 {
		return 0
	}
	return float64(s.SessionRemoteBytes) / float64(s.Size) * 100
}

// Amplification is network bytes moved per distinct byte the workload needed.
// Block granularity and prefetch push it above 1; we want it near 1 without
// collapsing into many tiny requests.
func (s Stats) Amplification() float64 {
	if s.WorkingSetBytes == 0 {
		return 0
	}
	return float64(s.SessionRemoteBytes) / float64(s.WorkingSetBytes)
}

// previousStats carries counters forward across processes, so that opening the
// same artifact twice reports what was ever downloaded for it rather than
// resetting to zero while the cache on disk keeps growing.
func previousStats(objectDir string, ident object.Identity) Stats {
	data, err := os.ReadFile(filepath.Join(objectDir, "stats.json"))
	if err != nil {
		return Stats{}
	}
	var s Stats
	if err := json.Unmarshal(data, &s); err != nil {
		return Stats{}
	}
	if s.URI != ident.URI || s.Size != ident.Size {
		return Stats{}
	}
	s.CachedBytes, s.UpdatedAt = 0, time.Time{}
	return s
}
