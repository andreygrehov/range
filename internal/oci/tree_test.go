package oci

import (
	"archive/tar"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/erofs/erofstest"
)

// The same through the OCI path: ownership comes from the layer headers, and
// the kernel sees it that way.
func TestOCITreeOwnershipInTheKernel(t *testing.T) {
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o640, Gid: 42}, "root:*:1::::::\n"),
		tarEntry(t, &tar.Header{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755}, "elf"),
		tarEntry(t, &tar.Header{Name: "home/app/data", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1234, Gid: 5678}, "x"),
		tarEntry(t, &tar.Header{Name: "Makefile", Typeflag: tar.TypeReg, Mode: 0o644}, "upper"),
		tarEntry(t, &tar.Header{Name: "makefile", Typeflag: tar.TypeReg, Mode: 0o644}, "lower"),
	)
	tree := newTree(t.TempDir())
	if err := tree.applyLayer(layer, 0); err != nil {
		t.Fatal(err)
	}
	root, nodes := tree.nodes()
	image := filepath.Join(t.TempDir(), "oci.erofs")
	if _, err := erofs.WriteNodes(root, nodes, image, 1<<20); err != nil {
		t.Fatal(err)
	}
	mnt := erofstest.KernelMount(t, image)
	for name, want := range map[string][3]uint32{
		"etc/shadow":    {0, 42, 0o640},
		"usr/bin/sudo":  {0, 0, 0o4755},
		"home/app/data": {1234, 5678, 0o600},
		"etc":           {0, 0, 0o755},
	} {
		info, err := os.Lstat(filepath.Join(mnt, name))
		if err != nil {
			t.Fatal(err)
		}
		st := info.Sys().(*syscall.Stat_t)
		if st.Uid != want[0] || st.Gid != want[1] || uint32(st.Mode)&0o7777 != want[2] {
			t.Errorf("%s: %d:%d %o, want %d:%d %o", name, st.Uid, st.Gid, uint32(st.Mode)&0o7777, want[0], want[1], want[2])
		}
	}
	upper, _ := os.ReadFile(filepath.Join(mnt, "Makefile"))
	lower, _ := os.ReadFile(filepath.Join(mnt, "makefile"))
	if string(upper) != "upper" || string(lower) != "lower" {
		t.Errorf("case variants: %q %q", upper, lower)
	}
}

// Three layers the way real images use them: usrmerge through a symlink, a
// directory whiteout, a whiteout of one name of a hardlinked pair, a replaced
// /etc/shadow, a block device with a minor above 255. Checked in the tree on
// every platform, then in the kernel and by fsck.erofs where they exist.
func TestOCITreeMergesLayersLikeARuntime(t *testing.T) {
	base := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "usr/lib/", Typeflag: tar.TypeDir, Mode: 0o755}, ""),
		tarEntry(t, &tar.Header{Name: "lib", Typeflag: tar.TypeSymlink, Linkname: "usr/lib"}, ""),
		tarEntry(t, &tar.Header{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755}, "elf"),
		tarEntry(t, &tar.Header{Name: "usr/bin/sudoedit", Typeflag: tar.TypeLink, Linkname: "usr/bin/sudo"}, ""),
		tarEntry(t, &tar.Header{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o640, Gid: 42}, "v1"),
		tarEntry(t, &tar.Header{Name: "old/a/b", Typeflag: tar.TypeReg, Mode: 0o644}, "x"),
		tarEntry(t, &tar.Header{Name: "dev/nvme0n1", Typeflag: tar.TypeBlock, Mode: 0o660, Gid: 6,
			Devmajor: 259, Devminor: 70000}, ""),
	)
	middle := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "lib/libnew.so", Typeflag: tar.TypeReg, Mode: 0o755}, "so"),
		tarEntry(t, &tar.Header{Name: ".wh.old", Typeflag: tar.TypeReg}, ""),
		tarEntry(t, &tar.Header{Name: "usr/bin/.wh.sudoedit", Typeflag: tar.TypeReg}, ""),
	)
	top := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o640, Gid: 42}, "v2"),
		tarEntry(t, &tar.Header{Name: "home/app/data", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1234, Gid: 5678}, "d"),
	)
	tree, find, e := buildTreeImage(t, base, middle, top)
	if _, ok := find("usr/lib/libnew.so"); !ok {
		t.Error("a file written through lib -> usr/lib is not in usr/lib")
	}
	if _, ok := find("old"); ok {
		t.Error("a whited-out directory survived")
	}
	if sudo, _ := find("usr/bin/sudo"); sudo.Nlink != 1 || sudo.Mode&0o7777 != 0o4755 {
		t.Errorf("sudo nlink %d mode %o, want 1 and 4755 after its second name was whited out", sudo.Nlink, sudo.Mode&0o7777)
	}
	if shadow, _ := find("etc/shadow"); string(e.Data(shadow)) != "v2" {
		t.Errorf("etc/shadow = %q, want the top layer's", e.Data(shadow))
	}
	dev, _ := find("dev/nvme0n1")
	rdev := dev.Blkaddr // i_u holds the device number for device inodes
	major, minor := (rdev&0xfff00)>>8, (rdev&0xff)|((rdev>>12)&0xfff00)
	if major != 259 || minor != 70000 || dev.Mode&syscall.S_IFMT != syscall.S_IFBLK {
		t.Errorf("block device = %d:%d mode %o, want 259:70000", major, minor, dev.Mode)
	}

	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("tree checked; the kernel half needs root on Linux")
	}
	root, nodes := tree.nodes()
	image := filepath.Join(t.TempDir(), "merged.erofs")
	if _, err := erofs.WriteNodes(root, nodes, image, 1<<20); err != nil {
		t.Fatal(err)
	}
	mnt := erofstest.KernelMount(t, image)
	info, err := os.Lstat(filepath.Join(mnt, "dev/nvme0n1"))
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	kmajor, kminor := (uint64(st.Rdev)&0xfff00)>>8, (uint64(st.Rdev)&0xff)|((uint64(st.Rdev)>>12)&0xfff00)
	if kmajor != 259 || kminor != 70000 {
		t.Errorf("kernel sees the device as %d:%d, want 259:70000", kmajor, kminor)
	}
	for name, want := range map[string]string{"usr/lib/libnew.so": "so", "etc/shadow": "v2"} {
		if got, err := os.ReadFile(filepath.Join(mnt, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(mnt, "old")); err == nil {
		t.Error("the kernel sees the whited-out directory")
	}
	if info, err := os.Stat(filepath.Join(mnt, "usr/bin/sudo")); err != nil || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		t.Error("the kernel sees sudo with more than one link")
	}
}

// A path written through a symlinked directory lands where the symlink points,
// the way unpacking onto disk through os.Root does, and the symlink survives.
func TestOCITreeFollowsSymlinkedDirectories(t *testing.T) {
	lower := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "usr/lib/", Typeflag: tar.TypeDir, Mode: 0o755}, ""),
		tarEntry(t, &tar.Header{Name: "lib", Typeflag: tar.TypeSymlink, Linkname: "usr/lib"}, ""),
		tarEntry(t, &tar.Header{Name: "abs", Typeflag: tar.TypeSymlink, Linkname: "/usr/lib"}, ""),
		tarEntry(t, &tar.Header{Name: "up", Typeflag: tar.TypeSymlink, Linkname: "../../../../usr/lib"}, ""),
	)
	upper := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "lib/libone.so", Typeflag: tar.TypeReg, Mode: 0o755}, "one"),
		tarEntry(t, &tar.Header{Name: "abs/libtwo.so", Typeflag: tar.TypeReg, Mode: 0o755}, "two"),
		tarEntry(t, &tar.Header{Name: "up/libthree.so", Typeflag: tar.TypeReg, Mode: 0o755}, "three"),
	)
	tree, find, _ := buildTreeImage(t, lower, upper)
	for _, name := range []string{"libone.so", "libtwo.so", "libthree.so"} {
		if _, ok := find("usr/lib/" + name); !ok {
			t.Errorf("usr/lib/%s missing: the write did not follow the symlink", name)
		}
	}
	if node, err := tree.lookup("lib", false); err != nil || node.Mode&syscall.S_IFMT != syscall.S_IFLNK {
		t.Error("the lib symlink was replaced by a directory")
	}
}

func TestOCITreeHardlinksShareAnInode(t *testing.T) {
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "usr/bin/python3.12", Typeflag: tar.TypeReg, Mode: 0o755}, "elf"),
		tarEntry(t, &tar.Header{Name: "usr/bin/python3", Typeflag: tar.TypeLink, Linkname: "usr/bin/python3.12"}, ""),
	)
	tree, find, _ := buildTreeImage(t, layer)
	a, _ := find("usr/bin/python3.12")
	b, _ := find("usr/bin/python3")
	if a.Blkaddr != b.Blkaddr || a.Nlink != 2 {
		t.Errorf("hardlinked names do not share one inode: %+v %+v", a, b)
	}
	if _, err := tree.lookup("usr/bin/python3", false); err != nil {
		t.Fatal(err)
	}
}
