package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A second open loads the kept layout and reads no layer and no index; a
// layer's index is read when that layer is.
func TestALaidOutImageOpensWithoutItsIndexes(t *testing.T) {
	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	indexes, _ := filepath.Glob(filepath.Join(dir, "*.idx"))
	if len(indexes) == 0 {
		t.Fatal("the first open kept no index")
	}
	for _, idx := range indexes {
		os.Remove(idx)
	}
	os.RemoveAll(filepath.Join(dir, "blobs"))
	reg.reset()

	l := NewLazy(name, HostPlatform(), dir)
	if _, err := l.Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	reg.mu.Lock()
	opened := reg.fullGets + reg.rangeGets
	reg.mu.Unlock()
	if opened != 0 {
		t.Fatalf("opening a laid-out image read layers %d times", opened)
	}
	img := readAll(t, l)
	for p, body := range want {
		if in, ok := find(img, p); !ok || string(img.Data(in)) != body {
			t.Errorf("%s reads back wrong", p)
		}
	}
}

// A kept layout that is damaged is laid out again, not served.
func TestADamagedLayoutIsLaidOutAgain(t *testing.T) {
	layers, want, _ := testLayers(t)
	_, name := serveImage(t, layers...)
	dir := t.TempDir()
	first := NewLazy(name, HostPlatform(), dir)
	info, err := first.Stat(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := filepath.Glob(filepath.Join(dir, "layouts", "*.img"))
	if len(kept) != 1 {
		t.Fatalf("%d layouts kept, want 1", len(kept))
	}
	data, _ := os.ReadFile(kept[0])
	data[len(data)/2] ^= 0xff
	os.WriteFile(kept[0], data, 0o644)

	l := NewLazy(name, HostPlatform(), dir)
	again, err := l.Stat(context.Background(), "")
	if err != nil || again.Size != info.Size {
		t.Fatalf("after damage: %+v, %v", again, err)
	}
	img := readAll(t, l)
	for p, body := range want {
		if in, ok := find(img, p); !ok || string(img.Data(in)) != body {
			t.Errorf("%s reads back wrong after the layout was damaged", p)
		}
	}
}

func TestOnlyTheLayoutsUsedLastStay(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 5; i++ {
		path := filepath.Join(dir, fmt.Sprintf("l%d.img", i))
		os.WriteFile(path, nil, 0o644)
		used := time.Now().Add(time.Duration(i-10) * time.Minute)
		os.Chtimes(path, used, used)
	}
	markUsed(filepath.Join(dir, "l0.img")) // the oldest, read just now
	pruneLayouts(dir, 3)
	var left []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if got := strings.Join(left, " "); got != "l0.img l3.img l4.img" {
		t.Fatalf("left %q", got)
	}
}
