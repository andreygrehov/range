package oci

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A layer read through ranged requests past a quarter of it is downloaded
// whole, once, in the background; reads then come from disk.
func TestALayerReadInPartIsDownloadedWhole(t *testing.T) {
	old := bulkMinSize
	bulkMinSize = 1 << 20
	t.Cleanup(func() { bulkMinSize = old })

	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// As if the index had come from the catalog: the layers are not here.
	os.RemoveAll(filepath.Join(dir, "blobs"))
	reg.reset()

	l := NewLazy(name, HostPlatform(), dir)
	img := readAll(t, l)
	l.bulk.Wait()
	for p, body := range want {
		if in, ok := find(img, p); !ok || string(img.Data(in)) != body {
			t.Errorf("%s reads back wrong", p)
		}
	}
	reg.mu.Lock()
	wholeReads := reg.fullGets
	reg.mu.Unlock()
	if wholeReads != 1 {
		t.Fatalf("%d whole downloads, want one: of the layer large enough", wholeReads)
	}
	big := layers[0] // the 3 MB layer; the other is under the minimum
	kept, err := os.ReadFile(l.blobPath(digestOf(big)))
	if err != nil || string(kept) != string(big) {
		t.Fatalf("the downloaded layer is not kept as served: %v", err)
	}

	reg.reset()
	readAll(t, l)
	reg.mu.Lock()
	defer reg.mu.Unlock()
	// Only the small layer, under the minimum, is still read from the registry.
	if reg.bytesSent >= int64(len(big)) {
		t.Errorf("after the download, reads still sent %d bytes in %d ranged requests", reg.bytesSent, reg.rangeGets)
	}
}

// A download that does not hash to the layer's digest is not kept.
func TestAWholeDownloadIsCheckedBeforeItIsKept(t *testing.T) {
	layers, _, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	dir := t.TempDir()
	l := NewLazy(name, HostPlatform(), dir)
	if _, err := l.Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(dir, "blobs"))
	d := digestOf(layers[0])
	reg.mu.Lock()
	bad := append([]byte(nil), layers[0]...)
	bad[len(bad)/2] ^= 0xff
	reg.blobs[d] = bad
	reg.mu.Unlock()

	r, err := resolve(context.Background(), name, HostPlatform())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.keepBlob(context.Background(), r.client, d, nil); err == nil {
		t.Fatal("a layer that does not match its digest was kept")
	}
	if _, err := os.Stat(l.blobPath(d)); !os.IsNotExist(err) {
		t.Fatal("the mismatched download was left in the blob store")
	}
	if partials, _ := filepath.Glob(filepath.Join(dir, "blobs", ".partial-*")); len(partials) != 0 {
		t.Fatalf("the failed download left %v", partials)
	}
}
