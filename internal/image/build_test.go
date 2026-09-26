package image

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/core/coretest"
)

func TestImageBuildValidation(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootfs, "marker"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "dev.img")

	tests := []struct {
		name string
		req  BuildRequest
		want string
	}{
		{"missing rootfs", BuildRequest{Rootfs: filepath.Join(t.TempDir(), "nope"), Size: 1 << 30, Output: output}, "no such file"},
		{"rootfs is a file", BuildRequest{Rootfs: filepath.Join(rootfs, "marker"), Size: 1 << 30, Output: output}, "not a directory"},
		{"empty rootfs", BuildRequest{Rootfs: t.TempDir(), Size: 1 << 30, Output: output}, "empty"},
		// --size only constrains ext4; EROFS is exactly as large as its contents.
		{"size too small", BuildRequest{Rootfs: rootfs, Size: 1 << 10, Output: output, FS: "ext4"}, "at least"},
		{"size not block aligned", BuildRequest{Rootfs: rootfs, Size: (1 << 30) + 1, Output: output, FS: "ext4"}, "multiple of 4096"},
		{"missing output", BuildRequest{Rootfs: rootfs, Size: 1 << 30}, "--output"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.req)
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
	// The same sizes are fine for EROFS, which never uses them.
	for _, size := range []int64{1 << 10, (1 << 30) + 1, 0} {
		req := BuildRequest{Rootfs: rootfs, Size: size, Output: output, FS: "erofs"}
		if err := validate(req); err != nil {
			t.Errorf("EROFS build rejected size %d: %v", size, err)
		}
	}

	if err := validate(BuildRequest{Rootfs: rootfs, Size: 1 << 30, Output: output}); err != nil {
		t.Fatalf("a valid request was rejected: %v", err)
	}
}

func TestImageBuildProducesMountableExt4(t *testing.T) {
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		t.Skip("mkfs.ext4 is not available on this host")
	}
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "workspace", "marker"), []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("workspace/marker", filepath.Join(rootfs, "link")); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "dev.img")
	req := BuildRequest{Rootfs: rootfs, Size: 64 << 20, Output: output, FS: "ext4"}
	if err := BuildNative(req); err != nil {
		t.Fatalf("buildImageNative: %v", err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != req.Size {
		t.Fatalf("image size = %d, want %d", info.Size(), req.Size)
	}
	// The image must be openable by Range itself.
	r, err := core.Open(context.Background(), output, coretest.Config(t, nil))
	if err != nil {
		t.Fatalf("range cannot open the image it built: %v", err)
	}
	defer r.Close()
	header := make([]byte, 4096)
	if _, err := r.ReadAt(header, 1024); err != nil {
		t.Fatal(err)
	}
	// ext4 superblock magic 0xEF53 sits at offset 0x38 of the superblock.
	if header[0x38] != 0x53 || header[0x39] != 0xEF {
		t.Fatalf("no ext4 superblock magic in the built image: %x", header[0x38:0x3a])
	}
}
