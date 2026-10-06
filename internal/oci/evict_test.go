package oci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Past the budget, the kept layers used least recently go first; the layer
// just added and a download in progress stay.
func TestKeptLayersFitTheBudget(t *testing.T) {
	l := NewLazy("x", HostPlatform(), t.TempDir())
	l.SetKeepLimit(250)
	dir := filepath.Join(l.dir, "blobs")
	os.MkdirAll(dir, 0o755)
	now := time.Now()
	for i, name := range []string{"oldest", "older", "recent", "just", ".partial-1"} {
		path := filepath.Join(dir, name)
		os.WriteFile(path, []byte(strings.Repeat("x", 100)), 0o644)
		used := now.Add(time.Duration(i-10) * time.Minute)
		os.Chtimes(path, used, used)
	}
	// Reading "oldest" makes it the most recently used.
	markUsed(filepath.Join(dir, "oldest"))

	l.evictKept(filepath.Join(dir, "just"))
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, " "); got != ".partial-1 just oldest" {
		t.Fatalf("left %q, want the in-progress download, the layer just added and the one read last", got)
	}
}
