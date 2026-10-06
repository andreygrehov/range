package virtual

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/erofs/erofstest"
)

// tree makes a root with one external file per body, plus one local file
// whose body the writer copies from disk, like environment.json.
func tree(t *testing.T, bodies map[string][]byte, local []byte) (*erofs.Node, []*erofs.Node, map[*erofs.Node][]byte) {
	t.Helper()
	root := &erofs.Node{Mode: syscall.S_IFDIR | 0o755, Nlink: 2, Mtime: time.Unix(0, 0)}
	root.Parent = root
	nodes := []*erofs.Node{root}
	data := map[*erofs.Node][]byte{}
	for name, body := range bodies {
		n := &erofs.Node{Mode: syscall.S_IFREG | 0o644, Nlink: 1, Size: int64(len(body)), Parent: root, External: true}
		root.Entries = append(root.Entries, erofs.Dirent{Name: name, Node: n})
		nodes = append(nodes, n)
		data[n] = body
	}
	path := filepath.Join(t.TempDir(), "local")
	if err := os.WriteFile(path, local, 0o644); err != nil {
		t.Fatal(err)
	}
	n := &erofs.Node{Mode: syscall.S_IFREG | 0o644, Nlink: 1, Size: int64(len(local)), Parent: root, Source: path}
	root.Entries = append(root.Entries, erofs.Dirent{Name: "local.json", Node: n})
	nodes = append(nodes, n)
	return root, nodes, data
}

func body(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func TestImageReadsBack(t *testing.T) {
	bodies := map[string][]byte{"big": body(1, 3<<20+5), "small": body(2, 100), "mid": body(3, 70000)}
	local := []byte(`{"shell": "/bin/sh"}`)
	root, nodes, data := tree(t, bodies, local)
	var reads atomic.Int64
	img, err := Build(root, nodes, 1<<20, func(n *erofs.Node) (ReadFunc, Tag) {
		return func(_ context.Context, dst []byte, off int64) error {
			reads.Add(1)
			copy(dst, data[n][off:])
			return nil
		}, Tag{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(img.Extents) != 3 {
		t.Fatalf("%d extents, want one per external file", len(img.Extents))
	}
	if img.MetadataSize() > 64<<10 {
		t.Errorf("%d bytes held in memory for three files", img.MetadataSize())
	}
	var all []byte
	for off := int64(0); off < img.Size; off += 1 << 20 {
		part, err := img.ReadRange(context.Background(), off, min(1<<20, img.Size-off))
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, part...)
	}
	fs := erofstest.Image{T: t, Bytes: all}
	rootIn := fs.Inode(uint64(binary.LittleEndian.Uint16(all[erofs.SuperOffset+14:])))
	found := 0
	for _, e := range fs.Readdir(rootIn) {
		want, ok := bodies[e.Name]
		if e.Name == "local.json" {
			want, ok = local, true
		}
		if !ok {
			continue
		}
		found++
		if got := fs.Data(fs.Inode(e.Nid)); !bytes.Equal(got, want) {
			t.Errorf("%s reads back wrong", e.Name)
		}
	}
	if found != 4 {
		t.Errorf("found %d of 4 files", found)
	}

	// The metadata and the local file never touch a reader.
	reads.Store(0)
	if _, err := img.ReadRange(context.Background(), 0, img.Extents[0].Start); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 0 {
		t.Errorf("reading metadata called %d readers", reads.Load())
	}
}

func TestAReadErrorFailsTheRange(t *testing.T) {
	root, nodes, _ := tree(t, map[string][]byte{"f": body(4, 5000)}, []byte("x"))
	boom := errors.New("gone")
	img, err := Build(root, nodes, 0, func(*erofs.Node) (ReadFunc, Tag) {
		return func(context.Context, []byte, int64) error { return boom }, Tag{}
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := img.ReadRange(context.Background(), img.Extents[0].Start, 100); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the reader's error", err)
	}
	if _, err := img.ReadRange(context.Background(), img.Size-1, 2); err == nil {
		t.Error("a read past the end succeeded")
	}
}

// A saved image loads back the same, and asks for its readers by tag. A cut
// or changed file is refused.
func TestSavedImageLoadsBack(t *testing.T) {
	bodies := map[string][]byte{"big": body(1, 3<<20+5), "small": body(2, 100)}
	root, nodes, data := tree(t, bodies, []byte(`{"shell": "/bin/sh"}`))
	byTag := map[Tag]*erofs.Node{}
	reader := func(n *erofs.Node) ReadFunc {
		return func(_ context.Context, dst []byte, off int64) error { copy(dst, data[n][off:]); return nil }
	}
	img, err := Build(root, nodes, 1<<20, func(n *erofs.Node) (ReadFunc, Tag) {
		tag := Tag{int64(len(byTag)), 7}
		byTag[tag] = n
		return reader(n), tag
	})
	if err != nil {
		t.Fatal(err)
	}
	var saved bytes.Buffer
	if err := img.Save(&saved); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(saved.Bytes(), func(tag Tag) ReadFunc { return reader(byTag[tag]) })
	if err != nil {
		t.Fatal(err)
	}
	want, _ := img.ReadRange(context.Background(), 0, img.Size)
	got, err := loaded.ReadRange(context.Background(), 0, loaded.Size)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("the loaded image reads back different bytes (%v)", err)
	}
	b := saved.Bytes()
	for _, bad := range [][]byte{b[:len(b)/2], append(append([]byte{}, b[:100]...), append([]byte{b[100] ^ 1}, b[101:]...)...), nil} {
		if _, err := Load(bad, func(Tag) ReadFunc { return nil }); err == nil {
			t.Fatal("a damaged saved image was loaded")
		}
	}
}
