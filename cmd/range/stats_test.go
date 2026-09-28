package main

import (
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/core/coretest"
	"github.com/andreygrehov/range/internal/rangetest"
)

func TestStatsPersistAndReload(t *testing.T) {
	data := rangetest.Data(256 << 10)
	cacheDir := t.TempDir()
	r, _ := coretest.NewReader(t, data, func(c *core.Config) { c.CacheDir = cacheDir })
	if _, err := r.ReadAt(make([]byte, 100<<10), 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := r.WriteStats(); err != nil {
		t.Fatalf("writeStats: %v", err)
	}

	found, err := loadAllStats(cacheDir)
	if err != nil {
		t.Fatalf("loadAllStats: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("loaded %d stats files, want 1", len(found))
	}
	s := found[0]
	if s.Size != int64(len(data)) {
		t.Fatalf("Size = %d, want %d", s.Size, len(data))
	}
	if s.RemoteBytes == 0 || s.CachedBytes == 0 {
		t.Fatalf("expected non-zero remote and cached bytes, got %d and %d", s.RemoteBytes, s.CachedBytes)
	}
	if s.WorkingSetRatio() <= 0 || s.WorkingSetRatio() > 100 {
		t.Fatalf("workingSetRatio = %.2f%%, want a value in (0,100]", s.WorkingSetRatio())
	}
}

func TestSessionIdentityReachesPersistedStats(t *testing.T) {
	data := rangetest.Data(128 << 10)
	cacheDir := t.TempDir()
	r, _ := coretest.NewReader(t, data, func(c *core.Config) { c.CacheDir = cacheDir })
	r.SessionID, r.SessionName, r.Workload = "a91c22", "acme-dev", "go-test"
	r.ReadyIn = 2810 * time.Millisecond
	if _, err := r.ReadAt(make([]byte, 1024), 0); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteStats(); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadAllStats(cacheDir)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("loadAllStats = %v, %v", loaded, err)
	}
	if loaded[0].SessionID != "a91c22" || loaded[0].Workload != "go-test" {
		t.Fatalf("session identity lost: %+v", loaded[0])
	}
	if time.Duration(loaded[0].ReadyNanos) != 2810*time.Millisecond {
		t.Fatalf("ReadyNanos = %d", loaded[0].ReadyNanos)
	}
	if loaded[0].WorkingSetBytes == 0 {
		t.Fatal("working set was not persisted")
	}
}
