package oci

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A second open of a tag is served from the cache, without the registry:
// here the registry stalls every manifest request, so asking it would hang.
func TestATagIsResolvedOnceThenFromTheCache(t *testing.T) {
	layers, _, _ := testLayers(t)
	f, name := serveImage(t, layers...)
	dir := t.TempDir()
	first, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	stall := make(chan struct{})
	f.mu.Lock()
	f.stall = stall
	answered := f.manifests
	f.mu.Unlock()
	defer refreshes.Wait()
	defer close(stall)

	opened := make(chan error, 1)
	var second struct{ etag string }
	go func() {
		info, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), "")
		second.etag = info.ETag
		opened <- err
	}()
	select {
	case err := <-opened:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second open waited for the registry")
	}
	if second.etag != first.ETag {
		t.Fatalf("the cached open names %q, the first %q", second.etag, first.ETag)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifests != answered {
		t.Fatalf("the registry answered %d manifest requests for the second open", f.manifests-answered)
	}
}

// A cache entry that does not match its digest is ignored, not trusted.
func TestATamperedTagCacheIsIgnored(t *testing.T) {
	layers, _, _ := testLayers(t)
	f, name := serveImage(t, layers...)
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "tags", "*.json"))
	if len(entries) != 1 {
		t.Fatalf("%d cache entries, want 1", len(entries))
	}
	data, _ := os.ReadFile(entries[0])
	// Change one byte inside the base64 manifest: it no longer hashes to its digest.
	i := len(data) / 3
	data[i] ^= 1
	os.WriteFile(entries[0], data, 0o644)

	f.mu.Lock()
	before := f.manifests
	f.mu.Unlock()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	refreshes.Wait()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.manifests == before {
		t.Fatal("a tampered cache entry was used instead of the registry")
	}
}
