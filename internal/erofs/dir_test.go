package erofs_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/erofs/erofstest"
	"github.com/andreygrehov/range/internal/rangetest"
)

// Same tree, same bytes: the layout is a function of the source and nothing
// else, which is what lets identical files produce identical chunks.
func TestEROFSWriterIsDeterministic(t *testing.T) {
	root := erofsTestTree(t)
	a := filepath.Join(t.TempDir(), "a.erofs")
	b := filepath.Join(t.TempDir(), "b.erofs")
	if _, err := erofs.WriteDir(root, a, 1<<20); err != nil {
		t.Fatal(err)
	}
	if _, err := erofs.WriteDir(root, b, 1<<20); err != nil {
		t.Fatal(err)
	}
	x, _ := os.ReadFile(a)
	y, _ := os.ReadFile(b)
	if !bytes.Equal(x, y) {
		t.Fatal("two builds of the same tree differ")
	}
}

// The writer's output is checked against the only reader that matters: the
// kernel. Every file type, ownership, the setuid bit, hardlinks, a file large
// enough to be chunk-aligned, and a directory big enough to span dirent blocks.
func TestEROFSMountsInTheKernel(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("mounting needs root on Linux; run the test binary with sudo in a Linux VM")
	}
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "usr/bin"), 0o755))
	must(os.MkdirAll(filepath.Join(src, "many"), 0o700))
	must(os.WriteFile(filepath.Join(src, "usr/bin/tool"), []byte("#!/bin/sh\n"), 0o755))
	must(os.Chmod(filepath.Join(src, "usr/bin/tool"), 0o4755))
	must(os.Link(filepath.Join(src, "usr/bin/tool"), filepath.Join(src, "usr/bin/tool2")))
	must(os.Symlink("usr/bin", filepath.Join(src, "bin")))
	must(os.WriteFile(filepath.Join(src, "big"), rangetest.Data(3<<20+17), 0o644))
	must(os.WriteFile(filepath.Join(src, "owned"), []byte("x"), 0o600))
	must(os.Lchown(filepath.Join(src, "owned"), 1234, 5678))
	must(os.WriteFile(filepath.Join(src, "empty"), nil, 0o644))
	must(syscall.Mkfifo(filepath.Join(src, "fifo"), 0o644))
	for i := 0; i < 400; i++ {
		must(os.WriteFile(filepath.Join(src, "many", fmt.Sprintf("file-with-a-long-name-%04d", i)), []byte{byte(i)}, 0o644))
	}
	image := filepath.Join(t.TempDir(), "x.erofs")
	if _, err := erofs.WriteDir(src, image, 1<<20); err != nil {
		t.Fatal(err)
	}
	mnt := erofstest.KernelMount(t, image)
	count := 0
	err := filepath.Walk(src, func(p string, want os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		got, err := os.Lstat(filepath.Join(mnt, rel))
		if err != nil {
			t.Errorf("%s: missing in the mounted image: %v", rel, err)
			return nil
		}
		count++
		ws, gs := want.Sys().(*syscall.Stat_t), got.Sys().(*syscall.Stat_t)
		if ws.Mode != gs.Mode || ws.Uid != gs.Uid || ws.Gid != gs.Gid || ws.Nlink != gs.Nlink {
			t.Errorf("%s: mode %o uid %d gid %d nlink %d, want %o %d %d %d",
				rel, gs.Mode, gs.Uid, gs.Gid, gs.Nlink, ws.Mode, ws.Uid, ws.Gid, ws.Nlink)
		}
		switch {
		case want.Mode().IsRegular():
			a, _ := os.ReadFile(p)
			b, err := os.ReadFile(filepath.Join(mnt, rel))
			if err != nil || !bytes.Equal(a, b) {
				t.Errorf("%s: contents differ (%v)", rel, err)
			}
		case want.Mode()&os.ModeSymlink != 0:
			a, _ := os.Readlink(p)
			b, _ := os.Readlink(filepath.Join(mnt, rel))
			if a != b {
				t.Errorf("%s: symlink %q, want %q", rel, b, a)
			}
		case want.IsDir():
			a, _ := os.ReadDir(p)
			b, _ := os.ReadDir(filepath.Join(mnt, rel))
			if len(a) != len(b) {
				t.Errorf("%s: %d entries, want %d", rel, len(b), len(a))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d paths identical in the kernel mount", count)
}
