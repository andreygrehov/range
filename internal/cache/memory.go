package cache

import (
	"container/list"
	"sync"
)

// Memory is a size-bounded LRU of blocks held in the process.
type Memory struct {
	mu      sync.Mutex
	limit   int64
	bytes   int64
	order   *list.List
	entries map[int64]*list.Element
}

type memoryEntry struct {
	block int64
	data  []byte
}

// NewMemory returns an empty cache that holds at most limit bytes.
func NewMemory(limit int64) *Memory {
	return &Memory{limit: limit, order: list.New(), entries: make(map[int64]*list.Element)}
}

// Get returns a cached block, or nil, and marks it recently used.
func (c *Memory) Get(block int64) []byte {
	if c.limit <= 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[block]
	if !ok {
		return nil
	}
	c.order.MoveToFront(element)
	return element.Value.(*memoryEntry).data
}

// Put caches a block, evicting the least recently used beyond the limit. A
// block larger than the whole cache is not kept.
func (c *Memory) Put(block int64, data []byte) {
	if c.limit <= 0 || int64(len(data)) > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[block]; ok {
		c.order.MoveToFront(element)
		return
	}
	c.entries[block] = c.order.PushFront(&memoryEntry{block: block, data: data})
	c.bytes += int64(len(data))
	for c.bytes > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(*memoryEntry)
		c.order.Remove(oldest)
		delete(c.entries, entry.block)
		c.bytes -= int64(len(entry.data))
	}
}
