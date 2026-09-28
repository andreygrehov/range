package erofs_test

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/erofs/erofstest"
)

func TestEROFSWriterRoundTrip(t *testing.T) {
	const align = 1 << 20
	root := erofsTestTree(t)
	output := filepath.Join(t.TempDir(), "fs.erofs")
	st, err := erofs.WriteDir(root, output, align)
	if err != nil {
		t.Fatalf("writeEROFS: %v", err)
	}
	img, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(img)) != st.Size || st.Size%erofs.BlockSize != 0 {
		t.Fatalf("image is %d bytes, stats say %d", len(img), st.Size)
	}
	super := img[erofs.SuperOffset:]
	if binary.LittleEndian.Uint32(super[0:]) != erofs.Magic || super[12] != erofs.BlockBits {
		t.Fatal("superblock magic or block size is wrong")
	}
	rootNid := uint64(binary.LittleEndian.Uint16(super[14:]))
	e := erofstest.Image{T: t, Bytes: img}

	seen := map[string]bool{}
	var walk func(nid uint64, rel string)
	walk = func(nid uint64, rel string) {
		in := e.Inode(nid)
		source := filepath.Join(root, rel)
		info, err := os.Lstat(source)
		if err != nil {
			t.Fatalf("%s in the image has no source: %v", rel, err)
		}
		seen[rel] = true
		if uint32(in.Mode)&0o7777 != uint32(info.Mode().Perm()) && info.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s: mode %o, want %o", rel, in.Mode&0o7777, info.Mode().Perm())
		}
		switch {
		case info.IsDir():
			entries := e.Readdir(in)
			subdirs := 0
			for i, entry := range entries {
				if i > 0 && entries[i-1].Name >= entry.Name {
					t.Fatalf("%s: entries out of order at %q", rel, entry.Name)
				}
				if entry.FT == erofs.FTDir && entry.Name != "." && entry.Name != ".." {
					subdirs++
				}
			}
			if in.Nlink != uint32(2+subdirs) {
				t.Errorf("%s: nlink %d, want %d", rel, in.Nlink, 2+subdirs)
			}
			for _, entry := range entries {
				if entry.Name == "." || entry.Name == ".." {
					continue
				}
				walk(entry.Nid, filepath.Join(rel, entry.Name))
			}
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(source)
			if string(e.Data(in)) != target {
				t.Errorf("%s: symlink target %q, want %q", rel, e.Data(in), target)
			}
		default:
			want, _ := os.ReadFile(source)
			if !bytes.Equal(e.Data(in), want) {
				t.Errorf("%s: contents differ", rel)
			}
			if in.Size >= align/4 && int64(in.Blkaddr)*erofs.BlockSize%align != 0 {
				t.Errorf("%s: large file starts at block %d, not on a chunk boundary", rel, in.Blkaddr)
			}
		}
	}
	walk(rootNid, ".")
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		rel, _ := filepath.Rel(root, path)
		if !seen[rel] {
			t.Errorf("%s is missing from the image", rel)
		}
		return nil
	})

	// A hardlink is one inode reached twice.
	usr := e.Readdir(e.Inode(rootNid))
	var usrNid uint64
	for _, entry := range usr {
		if entry.Name == "usr" {
			usrNid = entry.Nid
		}
	}
	var bin []erofstest.Entry
	for _, entry := range e.Readdir(e.Inode(usrNid)) {
		if entry.Name == "bin" {
			bin = e.Readdir(e.Inode(entry.Nid))
		}
	}
	nids := map[string]uint64{}
	for _, entry := range bin {
		nids[entry.Name] = entry.Nid
	}
	if nids["tool"] == 0 || nids["tool"] != nids["tool-link"] {
		t.Errorf("hardlinked files have different inodes: %v", nids)
	}
	if e.Inode(nids["tool"]).Nlink != 2 {
		t.Errorf("hardlinked inode nlink = %d, want 2", e.Inode(nids["tool"]).Nlink)
	}
	if st.AlignedFiles != 1 {
		t.Errorf("aligned files = %d, want 1", st.AlignedFiles)
	}
}
