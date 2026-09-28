package oci

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/erofs/erofstest"
)

// tarLayer builds one image layer in memory. Every entry is written exactly as
// given, so a test can ship the malicious shapes a real registry could.
func tarLayer(t *testing.T, entries ...func(*tar.Writer)) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, entry := range entries {
		entry(tw)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func tarEntry(t *testing.T, h *tar.Header, body string) func(*tar.Writer) {
	return func(tw *tar.Writer) {
		t.Helper()
		h.Size = int64(len(body))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// buildTreeImage applies layers to an in-memory tree, writes EROFS and returns
// a resolver from path to inode, so a test can check what the kernel would see.
func buildTreeImage(t *testing.T, layers ...*bytes.Buffer) (*tree, func(string) (erofstest.Inode, bool), erofstest.Image) {
	t.Helper()
	blobs := t.TempDir()
	tree := newTree(blobs)
	for i, layer := range layers {
		if err := tree.applyLayer(layer, i); err != nil {
			t.Fatalf("layer %d: %v", i, err)
		}
	}
	root, nodes := tree.nodes()
	output := filepath.Join(t.TempDir(), "tree.erofs")
	if _, err := erofs.WriteNodes(root, nodes, output, 1<<20); err != nil {
		t.Fatal(err)
	}
	img, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	e := erofstest.Image{T: t, Bytes: img}
	rootNid := uint64(binary.LittleEndian.Uint16(img[erofs.SuperOffset+14:]))
	find := func(p string) (erofstest.Inode, bool) {
		nid := rootNid
		for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
			if part == "" {
				continue
			}
			found := false
			for _, entry := range e.Readdir(e.Inode(nid)) {
				if entry.Name == part {
					nid, found = entry.Nid, true
					break
				}
			}
			if !found {
				return erofstest.Inode{}, false
			}
		}
		return e.Inode(nid), true
	}
	return tree, find, e
}
