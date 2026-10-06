// Package vm boots the small Linux VM Range runs a session in.
//
// The VM has no disk image of its own. It boots a kernel and an initramfs,
// and the environment arrives as a block device served by Range on the host,
// over NBD. Apple's Virtualization.framework boots it on a Mac, QEMU with KVM
// on Linux. The kernel, its modules and a static busybox come from one
// archive, built by scripts/vm-assets.sh, downloaded once and checked against
// the SHA-256 below. The initramfs is put together here, with the Linux build
// of Range inside it.
package vm

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// The archives scripts/vm-assets.sh builds, one for each architecture, and
// where they are published.
var archives = map[string]struct{ name, sha256 string }{
	"arm64": {"range-vm-6.12.107+deb13-cloud-arm64.tar.gz", "28486894c7df92e27c42e37390fd56d4959696484e719479ddd061dd982ae07b"},
	"amd64": {"range-vm-6.12.107+deb13-cloud-amd64.tar.gz", "9809d779b710a425f12c6cfaebb11033518768544b855c82e920ac737b2d09a3"},
}

const releaseURL = "https://github.com/andreygrehov/range/releases/download/vm-2/"

// Kernel is the kernel in an assets directory.
func Kernel(assets string) string { return filepath.Join(assets, "kernel") }

// Assets returns the directory holding the kernel, busybox and the modules
// for this machine's architecture, fetching and unpacking the archive on
// first use. RANGE_VM_ASSETS names a local copy of the archive instead,
// which is still checked.
func Assets(ctx context.Context, cacheDir string) (string, error) {
	archive, ok := archives[runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("Range has no Linux VM for %s", runtime.GOARCH)
	}
	dir := filepath.Join(cacheDir, "vm", archive.sha256[:16])
	if _, err := os.Stat(Kernel(dir)); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	file, err := os.CreateTemp(filepath.Dir(dir), ".partial-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if local := os.Getenv("RANGE_VM_ASSETS"); local != "" {
		src, err := os.Open(local)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(file, src)
		src.Close()
		if err != nil {
			return "", err
		}
	} else {
		fmt.Fprintf(os.Stderr, "range: downloading the Linux VM (%s, first run only)\n", archive.name)
		if err := download(ctx, releaseURL+archive.name, file); err != nil {
			return "", fmt.Errorf("download the Linux VM: %w", err)
		}
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != archive.sha256 {
		return "", fmt.Errorf("the Linux VM archive has SHA-256 %s, want %s", got, archive.sha256)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(dir), ".unpack-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := unpack(file, staging); err != nil {
		return "", fmt.Errorf("unpack the Linux VM: %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		// Another session unpacked it first.
		if _, statErr := os.Stat(Kernel(dir)); statErr == nil {
			return dir, nil
		}
		return "", err
	}
	return dir, nil
}

func download(ctx context.Context, url string, w io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

// unpack extracts the regular files of a gzip'd tar into dir. The archive is
// checked before this runs, but a path that leaves dir is still refused.
func unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q", h.Name)
		}
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, tr)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
}
