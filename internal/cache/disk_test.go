package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A block that was truncated by a crash must be treated as absent, not served
// forever as a short read at that offset.
func TestDiskCacheRejectsAndDeletesShortBlock(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDisk(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	full := make([]byte, 64)
	if err := c.Put(7, full); err != nil {
		t.Fatal(err)
	}
	// Simulate the survivor of a partial write.
	if err := os.WriteFile(c.path(7), full[:10], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get(7, 64); ok {
		t.Fatal("a short block was served as a cache hit")
	}
	if _, err := os.Stat(c.path(7)); !os.IsNotExist(err) {
		t.Fatal("a short block was left on disk to fail again")
	}
}

// Eviction must drop what has not been read, not what was fetched first: the
// blocks fetched first are the boot working set.
func TestDiskCacheEvictsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	// 500 bytes holds five blocks; two more force eviction of the two oldest.
	c, err := NewDisk(dir, 500)
	if err != nil {
		t.Fatal(err)
	}
	for block := int64(0); block < 4; block++ {
		if err := c.Put(block, make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Read the oldest block, which under a fetch-order policy would be first out.
	stale := time.Now().Add(-2 * touchInterval)
	for block := int64(0); block < 4; block++ {
		os.Chtimes(c.path(block), stale, stale.Add(time.Duration(block)*time.Second))
	}
	if _, ok := c.Get(0, 100); !ok {
		t.Fatal("block 0 missing before eviction")
	}
	for block := int64(4); block < 6; block++ {
		if err := c.Put(block, make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
	}
	if !c.Has(0) {
		t.Error("eviction dropped the block that had just been read")
	}
	if c.Has(1) {
		t.Error("eviction kept an untouched block older than the one read")
	}
}

func TestDiskCacheRoundTripAndEviction(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDisk(dir, 1000)
	if err != nil {
		t.Fatalf("newDiskCache: %v", err)
	}
	if c.Has(0) {
		t.Fatal("empty cache reported a block")
	}
	payload := []byte("hello range")
	if err := c.Put(0, payload); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, ok := c.Get(0, int64(len(payload)))
	if !ok || string(got) != string(payload) {
		t.Fatalf("get = (%q, %v), want (%q, true)", got, ok, payload)
	}
	if c.Used() != int64(len(payload)) {
		t.Fatalf("used = %d, want %d", c.Used(), len(payload))
	}

	// Push well past the limit and confirm eviction brings it back under.
	for block := int64(1); block <= 20; block++ {
		if err := c.Put(block, make([]byte, 100)); err != nil {
			t.Fatalf("put %d: %v", block, err)
		}
		time.Sleep(time.Millisecond) // keep modification times distinguishable
	}
	if used := c.Used(); used > 1000 {
		t.Fatalf("cache holds %d bytes after eviction, limit is 1000", used)
	}
	if !c.Has(20) {
		t.Fatal("the most recent block should have survived eviction")
	}
}

func TestDiskCacheLeavesNoPartialBlocks(t *testing.T) {
	dir := t.TempDir()
	c, err := NewDisk(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Put(7, []byte("complete")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "blocks"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".tmp-") {
			t.Fatalf("temporary file %q left behind", entry.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly one block file, got %d", len(entries))
	}
}
