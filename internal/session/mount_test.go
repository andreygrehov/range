package session

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreygrehov/range/internal/erofs"
)

func TestOverlayOptions(t *testing.T) {
	got := overlayOptions("/run/range/a/lower", "/run/range/a/upper", "/run/range/a/work")
	want := "lowerdir=/run/range/a/lower,upperdir=/run/range/a/upper,workdir=/run/range/a/work"
	if got != want {
		t.Fatalf("overlayOptions = %q, want %q", got, want)
	}
}

// mount is told the filesystem instead of probing for it.
func TestFilesystemOfReadsTheSuperblockMagic(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "hello"), []byte("hi"), 0o644)
	image := filepath.Join(t.TempDir(), "x.erofs")
	if _, err := erofs.WriteDir(dir, image, 0); err != nil {
		t.Fatal(err)
	}
	erofs, _ := os.ReadFile(image)
	ext4 := make([]byte, 4096)
	binary.LittleEndian.PutUint16(ext4[1024+56:], 0xef53)
	for name, tc := range map[string]struct {
		img  []byte
		want string
	}{
		"erofs": {erofs, "erofs"},
		"ext4":  {ext4, "ext4"},
		"zeros": {make([]byte, 4096), ""},
		"short": {make([]byte, 100), ""},
	} {
		got, err := filesystemOf(bytes.NewReader(tc.img))
		if got != tc.want || (tc.want == "") != (err != nil) {
			t.Errorf("%s: got %q, %v; want %q", name, got, err, tc.want)
		}
	}
}
