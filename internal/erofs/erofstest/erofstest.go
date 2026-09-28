// Package erofstest reads EROFS images back the way the kernel does -
// superblock, extended inodes, dirent blocks - so writer output can be checked
// on any platform, and mounts them with the real kernel where it can.
package erofstest

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/tool"
)

// Image is a test-side reader that walks an image the way the kernel
// does: superblock, root_nid, extended inodes, dirent blocks, raw_blkaddr. It
// exists so the writer can be checked on any platform, not only on Linux.
type Image struct {
	T     *testing.T
	Bytes []byte
}

// Inode is the part of an extended inode the tests check.
type Inode struct {
	Mode     uint16
	Size     int64
	Blkaddr  uint32
	UID, GID uint32
	Nlink    uint32
	Mtime    int64
}

// Inode decodes the extended inode at nid, failing the test on any other format.
func (e Image) Inode(nid uint64) Inode {
	at := nid << erofs.SlotBits
	raw := e.Bytes[at : at+erofs.InodeSize]
	if binary.LittleEndian.Uint16(raw[0:]) != erofs.FormatExtended {
		e.T.Fatalf("nid %d: i_format %#x, want extended flat-plain", nid, raw[0])
	}
	return Inode{
		Mode: binary.LittleEndian.Uint16(raw[4:]), Size: int64(binary.LittleEndian.Uint64(raw[8:])),
		Blkaddr: binary.LittleEndian.Uint32(raw[16:]), UID: binary.LittleEndian.Uint32(raw[24:]),
		GID: binary.LittleEndian.Uint32(raw[28:]), Mtime: int64(binary.LittleEndian.Uint64(raw[32:])),
		Nlink: binary.LittleEndian.Uint32(raw[44:]),
	}
}

// Data returns a flat-plain inode's bytes.
func (e Image) Data(in Inode) []byte {
	at := int64(in.Blkaddr) * erofs.BlockSize
	return e.Bytes[at : at+in.Size]
}

// Entry is one directory entry.
type Entry struct {
	Name string
	Nid  uint64
	FT   uint8
}

// Readdir lists a directory inode's entries in on-disk order.
func (e Image) Readdir(in Inode) []Entry {
	var out []Entry
	raw := e.Data(in)
	for pos := 0; pos < len(raw); pos += erofs.BlockSize {
		end := pos + erofs.BlockSize
		if end > len(raw) {
			end = len(raw)
		}
		block := raw[pos:end]
		count := int(binary.LittleEndian.Uint16(block[8:])) / erofs.DirentSize
		for i := 0; i < count; i++ {
			d := block[i*erofs.DirentSize:]
			off := int(binary.LittleEndian.Uint16(d[8:]))
			stop := len(block)
			if i+1 < count {
				stop = int(binary.LittleEndian.Uint16(block[(i+1)*erofs.DirentSize+8:]))
			} else if nul := bytes.IndexByte(block[off:], 0); nul >= 0 {
				stop = off + nul
			}
			out = append(out, Entry{
				Name: string(block[off:stop]), Nid: binary.LittleEndian.Uint64(d[0:]), FT: d[10],
			})
		}
	}
	return out
}

// KernelMount mounts an EROFS image read-only through a loop device, or skips
// the test when this host cannot: the check needs Linux, root and the module.
func KernelMount(t *testing.T, image string) string {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("mounting needs root on Linux; run the test binary with sudo in a Linux VM")
	}
	if !tool.KernelHasFilesystem("erofs") {
		exec.Command("modprobe", "erofs").Run()
	}
	mnt := t.TempDir()
	if out, err := exec.Command("mount", "-t", "erofs", "-o", "ro,loop", image, mnt).CombinedOutput(); err != nil {
		t.Skipf("kernel cannot mount erofs here: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("umount", mnt).Run() })
	// The kernel accepting an image is not the whole check: fsck.erofs, where
	// it is installed, reads every inode and extent the way a mount only might.
	if fsck, err := exec.LookPath("fsck.erofs"); err == nil {
		if out, err := exec.Command(fsck, image).CombinedOutput(); err != nil {
			t.Errorf("fsck.erofs: %v\n%s", err, out)
		}
	}
	return mnt
}
