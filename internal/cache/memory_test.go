package cache

import (
	"testing"
)

func TestMemoryCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewMemory(300)
	c.Put(1, make([]byte, 100))
	c.Put(2, make([]byte, 100))
	c.Put(3, make([]byte, 100))
	if c.Get(1) == nil {
		t.Fatal("block 1 should still be cached")
	}
	// Block 1 is now most recently used, so block 2 is the eviction victim.
	c.Put(4, make([]byte, 100))
	if c.Get(2) != nil {
		t.Fatal("block 2 should have been evicted")
	}
	if c.Get(1) == nil || c.Get(3) == nil || c.Get(4) == nil {
		t.Fatal("blocks 1, 3 and 4 should be cached")
	}
	if c.bytes > c.limit {
		t.Fatalf("cache holds %d bytes, limit is %d", c.bytes, c.limit)
	}
}

func TestMemoryCacheRejectsOversizedBlock(t *testing.T) {
	c := NewMemory(100)
	c.Put(1, make([]byte, 200))
	if c.Get(1) != nil {
		t.Fatal("a block larger than the whole cache should not be stored")
	}
}
