package image

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/oci"
	"github.com/andreygrehov/range/internal/tool"
)

const minimumSize = 16 << 20

// BuildRequest describes one filesystem image to build.
type BuildRequest struct {
	Rootfs  string
	Size    int64 // ext4 only; an EROFS image is exactly as large as its contents
	Output  string
	FromOCI string
	// Platform the image is pulled for; the host's unless --platform says
	Platform oci.Platform
	FS       string // "erofs" (default) or "ext4"
	Align    int64  // artifact chunk size, so large files start on a chunk boundary
}

func validate(req BuildRequest) error {
	if req.Output == "" {
		return errors.New("image build: missing --output")
	}
	if req.Rootfs == "" && req.FromOCI != "" && req.FS != "ext4" {
		return nil // built straight from the image; there is no directory to check
	}
	if req.Rootfs == "" {
		return errors.New("image build: missing <rootfs-dir>")
	}
	info, err := os.Stat(req.Rootfs)
	if err != nil {
		return fmt.Errorf("image build: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("image build: %s is not a directory", req.Rootfs)
	}
	entries, err := os.ReadDir(req.Rootfs)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return fmt.Errorf("image build: %s is empty", req.Rootfs)
	}
	if req.FS == "ext4" {
		if req.Size < minimumSize {
			return fmt.Errorf("image build: --size must be at least %s", bytesize.Format(minimumSize))
		}
		if req.Size%4096 != 0 {
			return fmt.Errorf("image build: --size must be a multiple of 4096 so the kernel can attach it")
		}
	}
	if req.Output == "" {
		return errors.New("image build: missing --output")
	}
	return nil
}

// BuildNative creates the image on this host. It needs e2fsprogs.
func BuildNative(req BuildRequest) error {
	if err := validate(req); err != nil {
		return err
	}
	if req.FS != "ext4" {
		if err := os.MkdirAll(filepath.Dir(req.Output), 0o755); err != nil {
			return err
		}
		var err error
		if req.Rootfs == "" && req.FromOCI != "" {
			_, err = oci.BuildEROFS(context.Background(), req.FromOCI, req.Output, req.Align, req.Platform)
		} else {
			_, err = erofs.WriteDir(req.Rootfs, req.Output, req.Align)
		}
		if err != nil {
			os.Remove(req.Output)
		}
		return err
	}
	if _, err := exec.LookPath("mkfs.ext4"); err != nil {
		return errors.New("image build needs mkfs.ext4 from e2fsprogs; install it with: sudo apt install e2fsprogs")
	}
	if err := os.MkdirAll(filepath.Dir(req.Output), 0o755); err != nil {
		return err
	}
	file, err := os.Create(req.Output)
	if err != nil {
		return err
	}
	// A sparse file: the logical size is what matters, not the bytes on disk.
	if err := file.Truncate(req.Size); err != nil {
		file.Close()
		os.Remove(req.Output)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(req.Output)
		return err
	}
	// -F: operate on a plain file. -d: populate from a directory.
	if err := tool.Run("mkfs.ext4", "-F", "-q", "-d", req.Rootfs, req.Output); err != nil {
		os.Remove(req.Output)
		return err
	}
	return nil
}
