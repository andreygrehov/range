package profile

import (
	"sort"
	"time"

	"github.com/andreygrehov/range/internal/object"
)

const version = 1

// timingBucketMillis quantises stored timings. Runs only merge when their
// bucketed timings agree, and a run stores the bucketed value, so expanding a
// profile and merging it again reproduces exactly what was stored instead of
// drifting a little further every session.
const timingBucketMillis = 10

func bucketMillis(ms int64) int64 { return ms - ms%timingBucketMillis }

// maxAge is how long a block stays in a profile without being demanded
// again. It bounds the working set to what the workload currently does.
const maxAge = 30 * 24 * time.Hour

// DefaultWorkload labels sessions that did not name what they were doing.
const DefaultWorkload = "interactive"

// BlockRange is a run of consecutive blocks plus what we have learned about it.
//
// Timing fields are milliseconds from the start of a session:
//
//	FirstSeenMillis  the earliest first-use ever observed for this run
//	MeanFirstUseMs   observation-weighted mean of per-session first use
//	Observations     how many sessions demanded this run
//	LastSeen         RFC3339 time of the most recent session that demanded it
type BlockRange struct {
	StartBlock      int64  `json:"startBlock"`
	Count           int64  `json:"count"`
	Observations    int64  `json:"observations"`
	FirstSeenMillis int64  `json:"firstSeenMs"`
	MeanFirstUseMs  int64  `json:"meanFirstUseMs"`
	LastSeen        string `json:"lastSeen"`
}

type artifact struct {
	URI       string `json:"uri"`
	Size      int64  `json:"size"`
	ETag      string `json:"etag"`
	VersionID string `json:"versionId,omitempty"`
}

// Profile is what previous sessions of one workload needed from one artifact.
// Unknown fields are ignored on read, so v1 readers tolerate additive changes.
type Profile struct {
	Version   int          `json:"version"`
	Artifact  artifact     `json:"artifact"`
	BlockSize int64        `json:"blockSize"`
	Workload  string       `json:"workload"`
	Sessions  int64        `json:"sessions"`
	UpdatedAt string       `json:"updatedAt"`
	Ranges    []BlockRange `json:"ranges"`
}

// BlockTotal is the number of blocks the profile covers.
func (p Profile) BlockTotal() int64 {
	var total int64
	for _, run := range p.Ranges {
		total += run.Count
	}
	return total
}

// blocks expands the runs in stored order.
func (p Profile) blocks() []int64 {
	out := make([]int64, 0, p.BlockTotal())
	for _, run := range p.Ranges {
		for i := int64(0); i < run.Count; i++ {
			out = append(out, run.StartBlock+i)
		}
	}
	return out
}

// PrefetchOrder ranks what to pull first: runs seen in the most sessions, then
// runs needed earliest, with stored order breaking ties. No model, just sorting.
func (p Profile) PrefetchOrder() []int64 {
	out := make([]int64, 0, p.BlockTotal())
	for _, run := range p.prefetchRuns() {
		for i := int64(0); i < run.Count; i++ {
			out = append(out, run.StartBlock+i)
		}
	}
	return out
}

// prefetchRuns is the same ranking with the runs left intact. The profile
// records contiguous runs for a reason: replaying them one block at a time
// turns a handful of large range requests into hundreds of small ones.
func (p Profile) prefetchRuns() []BlockRange {
	type ranked struct {
		run   BlockRange
		index int
	}
	runs := make([]ranked, len(p.Ranges))
	for i, run := range p.Ranges {
		runs[i] = ranked{run: run, index: i}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		a, b := runs[i].run, runs[j].run
		if a.Observations != b.Observations {
			return a.Observations > b.Observations
		}
		if a.MeanFirstUseMs != b.MeanFirstUseMs {
			return a.MeanFirstUseMs < b.MeanFirstUseMs
		}
		return runs[i].index < runs[j].index
	})
	out := make([]BlockRange, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.run)
	}
	return out
}

// matches refuses a profile recorded against a different artifact version,
// block size or workload. Replaying one would prefetch meaningless offsets.
func (p Profile) matches(ident object.Identity, blockSize int64, workload string) bool {
	return p.Version == version && p.BlockSize == blockSize &&
		p.Workload == workload &&
		p.Artifact.URI == ident.URI && p.Artifact.Size == ident.Size &&
		p.Artifact.ETag == ident.ETag && p.Artifact.VersionID == ident.VersionID
}

// blockStat is the per-block accumulation used while merging. Runs are
// re-derived from it afterwards.
type blockStat struct {
	observations int64
	firstSeenMs  int64
	firstUseSum  int64 // sum of per-session first use, for the mean
	lastSeen     time.Time
}

func (p Profile) expand() map[int64]*blockStat {
	out := make(map[int64]*blockStat, p.BlockTotal())
	for _, run := range p.Ranges {
		seen, _ := time.Parse(time.RFC3339, run.LastSeen)
		for i := int64(0); i < run.Count; i++ {
			out[run.StartBlock+i] = &blockStat{
				observations: run.Observations,
				firstSeenMs:  run.FirstSeenMillis,
				firstUseSum:  run.MeanFirstUseMs * run.Observations,
				lastSeen:     seen,
			}
		}
	}
	return out
}
