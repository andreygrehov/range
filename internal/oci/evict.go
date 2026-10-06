package oci

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The layers Range keeps, read whole to index them or downloaded whole by a
// session that read much of them, share one budget: the cache size. Past
// it, the layers used least recently go first. A session reading a layer
// that goes keeps reading it: its file stays open.

// defaultKeepLimit is the budget when none is set.
const defaultKeepLimit = 10 << 30

// SetKeepLimit sets the budget of kept layers, in bytes.
func (l *Lazy) SetKeepLimit(bytes int64) { l.keepLimit = bytes }

// markUsed records that a kept layer is being read, for evictKept.
func markUsed(path string) {
	now := time.Now()
	os.Chtimes(path, now, now)
}

// evictKept removes the least recently used kept layers until they fit the
// budget, sparing just, the layer that was just added.
func (l *Lazy) evictKept(just string) {
	limit := l.keepLimit
	if limit <= 0 {
		limit = defaultKeepLimit
	}
	dir := filepath.Join(l.dir, "blobs")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type kept struct {
		path string
		size int64
		used time.Time
	}
	var layers []kept
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue // a download in progress is not kept yet
		}
		layers = append(layers, kept{filepath.Join(dir, e.Name()), info.Size(), info.ModTime()})
		total += info.Size()
	}
	sort.Slice(layers, func(i, j int) bool { return layers[i].used.Before(layers[j].used) })
	for _, k := range layers {
		if total <= limit {
			return
		}
		if k.path == just {
			continue
		}
		if os.Remove(k.path) == nil {
			total -= k.size
		}
	}
}
