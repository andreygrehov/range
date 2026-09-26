package profile

import (
	"fmt"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/object"
)

func TestProfileMergeAccumulatesAcrossSessions(t *testing.T) {
	ident := testIdentity()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)

	first := merge(Profile{}, ident, 1<<20, "go-test", observe(map[int64]int{1: 50, 2: 60}), now)
	if first.Sessions != 1 {
		t.Fatalf("Sessions = %d after one session, want 1", first.Sessions)
	}
	if first.BlockTotal() != 2 {
		t.Fatalf("blockTotal = %d, want 2", first.BlockTotal())
	}

	// A second session sees one of the same blocks plus a new one.
	second := merge(first, ident, 1<<20, "go-test", observe(map[int64]int{2: 20, 9: 80}), now.Add(time.Hour))
	if second.Sessions != 2 {
		t.Fatalf("Sessions = %d, want 2", second.Sessions)
	}
	if second.BlockTotal() != 3 {
		t.Fatalf("blockTotal = %d, want 3 (union of both sessions)", second.BlockTotal())
	}

	byBlock := map[int64]BlockRange{}
	for _, run := range second.Ranges {
		for i := int64(0); i < run.Count; i++ {
			byBlock[run.StartBlock+i] = run
		}
	}
	if got := byBlock[2].Observations; got != 2 {
		t.Fatalf("block 2 observations = %d, want 2", got)
	}
	if got := byBlock[1].Observations; got != 1 {
		t.Fatalf("block 1 observations = %d, want 1", got)
	}
	// Block 2 was first used at 60ms, then at 20ms: the minimum must survive.
	if got := byBlock[2].FirstSeenMillis; got != 20 {
		t.Fatalf("block 2 firstSeenMs = %d, want 20", got)
	}
	if got := byBlock[2].MeanFirstUseMs; got != 40 {
		t.Fatalf("block 2 meanFirstUseMs = %d, want 40 (mean of 60 and 20)", got)
	}
	if byBlock[9].LastSeen == "" {
		t.Fatal("lastSeen was not recorded")
	}
}

func TestPrefetchOrderPrefersCommonThenEarly(t *testing.T) {
	p := Profile{Ranges: []BlockRange{
		{StartBlock: 10, Count: 1, Observations: 1, MeanFirstUseMs: 5},
		{StartBlock: 20, Count: 1, Observations: 9, MeanFirstUseMs: 900},
		{StartBlock: 30, Count: 1, Observations: 9, MeanFirstUseMs: 10},
	}}
	got := p.PrefetchOrder()
	want := []int64{30, 20, 10} // most sessions first, then earliest use
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("prefetchOrder = %v, want %v", got, want)
	}
}

// A profile that only ever unions keeps prefetching blocks the workload
// stopped touching months ago.
func TestProfileForgetsStaleBlocks(t *testing.T) {
	ident := object.Identity{URI: "s3://b/k", Size: 1 << 20, ETag: "e"}
	now := time.Now()
	old := merge(Profile{}, ident, 4096, "w",
		map[int64]time.Duration{1: 0, 2: 0}, now.Add(-maxAge-24*time.Hour))
	if old.BlockTotal() != 2 {
		t.Fatalf("first session recorded %d blocks, want 2", old.BlockTotal())
	}
	// A later session touches only block 2; block 1 has aged out.
	fresh := merge(old, ident, 4096, "w", map[int64]time.Duration{2: 0}, now)
	covered := map[int64]bool{}
	for _, run := range fresh.Ranges {
		for i := int64(0); i < run.Count; i++ {
			covered[run.StartBlock+i] = true
		}
	}
	if covered[1] {
		t.Error("a block untouched for longer than profileMaxAge was kept")
	}
	if !covered[2] {
		t.Error("the block this session touched was dropped")
	}
}
