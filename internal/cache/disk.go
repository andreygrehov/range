package cache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A block on disk is either absent or complete. Downloads land in a temporary
// file and are renamed into place, so a partial block is never observable.
type Disk struct {
	Dir   string
	limit int64
	mu    sync.Mutex
	bytes int64
}

// NewDisk opens a block cache in dir, bounded to limit bytes, and accounts
// for whatever an earlier process left there.
func NewDisk(dir string, limit int64) (*Disk, error) {
	blocks := filepath.Join(dir, "blocks")
	if err := os.MkdirAll(blocks, 0o755); err != nil {
		return nil, err
	}
	c := &Disk{Dir: dir, limit: limit}
	used, err := c.Scan()
	if err != nil {
		return nil, err
	}
	c.bytes = used
	return c, nil
}

// Scan totals the bytes already cached in the directory.
func (c *Disk) Scan() (int64, error) {
	entries, err := os.ReadDir(filepath.Join(c.Dir, "blocks"))
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return total, nil
}

func (c *Disk) path(block int64) string {
	return filepath.Join(c.Dir, "blocks", fmt.Sprintf("%016d", block))
}

// Has reports whether a block is cached, without reading it.
func (c *Disk) Has(block int64) bool {
	_, err := os.Stat(c.path(block))
	return err == nil
}

// touchInterval follows relatime: an access timestamp is only rewritten once
// it is already stale, so a hot block does not pay a syscall on every read.
const touchInterval = time.Minute

// Get returns a cached block, but only if it is exactly the length the caller
// expects. A short block - a partial write that survived a crash - would
// otherwise become a permanent "short read" at that offset, so it is deleted
// and reported as a miss.
func (c *Disk) Get(block, want int64) ([]byte, bool) {
	path := c.path(block)
	file, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false
	}
	if want > 0 && info.Size() != want {
		file.Close()
		c.drop(path, info.Size())
		return nil, false
	}
	data := make([]byte, info.Size())
	if _, err := io.ReadFull(file, data); err != nil {
		file.Close()
		c.drop(path, info.Size())
		return nil, false
	}
	// Eviction sorts by modification time. Without this the oldest timestamp
	// belongs to whatever was fetched first, which is the boot working set -
	// precisely the blocks worth keeping.
	if time.Since(info.ModTime()) >= touchInterval {
		now := time.Now()
		os.Chtimes(path, now, now)
	}
	return data, true
}

func (c *Disk) drop(path string, size int64) {
	if err := os.Remove(path); err != nil {
		return
	}
	c.mu.Lock()
	c.bytes -= size
	c.mu.Unlock()
}

// Put stores a block atomically, so a crash never leaves a partial one, and
// evicts the least recently used blocks beyond the limit.
func (c *Disk) Put(block int64, data []byte) error {
	target := c.path(block)
	temp, err := os.CreateTemp(filepath.Join(c.Dir, "blocks"), ".tmp-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	// Rename publishes the block, so the contents have to reach the platter
	// first; otherwise a crash leaves a visible, empty, permanently wrong block.
	if err := temp.Sync(); err != nil {
		temp.Close()
		os.Remove(name)
		return err
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, target); err != nil {
		os.Remove(name)
		return err
	}
	c.mu.Lock()
	c.bytes += int64(len(data))
	over := c.limit > 0 && c.bytes > c.limit
	c.mu.Unlock()
	if over {
		c.evict()
	}
	return nil
}

// evict deletes the least recently used blocks until the cache is back under
// 90% of its limit. get keeps the timestamps meaningful.
func (c *Disk) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()
	dir := filepath.Join(c.Dir, "blocks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type aged struct {
		name string
		size int64
		age  time.Time
	}
	files := make([]aged, 0, len(entries))
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || strings.HasPrefix(entry.Name(), ".tmp-") {
			continue
		}
		files = append(files, aged{entry.Name(), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(files, func(i, j int) bool { return files[i].age.Before(files[j].age) })
	target := c.limit - c.limit/10
	for _, file := range files {
		if total <= target {
			break
		}
		if err := os.Remove(filepath.Join(dir, file.name)); err == nil {
			total -= file.size
		}
	}
	c.bytes = total
}

// Used reports the bytes currently cached.
func (c *Disk) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}
