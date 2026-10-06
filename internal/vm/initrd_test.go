package vm

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

type cpioEntry struct {
	mode uint64
	rdev [2]uint64
	data []byte
}

// readCPIO parses a "newc" archive the way the kernel does.
func readCPIO(t *testing.T, b []byte) map[string]cpioEntry {
	t.Helper()
	entries := map[string]cpioEntry{}
	field := func(h []byte, i int) uint64 {
		v, err := strconv.ParseUint(string(h[6+8*i:14+8*i]), 16, 64)
		if err != nil {
			t.Fatalf("bad header field %d: %v", i, err)
		}
		return v
	}
	align := func(n int) int { return (n + 3) &^ 3 }
	for off := 0; ; {
		if off+110 > len(b) || string(b[off:off+6]) != "070701" {
			t.Fatalf("no header at %d", off)
		}
		h := b[off : off+110]
		nameSize, size := int(field(h, 11)), int(field(h, 6))
		name := string(b[off+110 : off+110+nameSize-1])
		dataAt := align(off + 110 + nameSize)
		if name == "TRAILER!!!" {
			return entries
		}
		entries[name] = cpioEntry{mode: field(h, 1), rdev: [2]uint64{field(h, 9), field(h, 10)},
			data: b[dataAt : dataAt+size]}
		off = align(dataAt + size)
	}
}

func TestInitrdHoldsWhatTheVMBootsFrom(t *testing.T) {
	assets := t.TempDir()
	os.MkdirAll(filepath.Join(assets, "modules"), 0o755)
	os.WriteFile(filepath.Join(assets, "busybox"), []byte("busybox!"), 0o755)
	os.WriteFile(filepath.Join(assets, "modules", "order"), []byte("erofs\noverlay\n"), 0o644)
	os.WriteFile(filepath.Join(assets, "modules", "erofs.ko"), []byte("erofs module"), 0o644)
	os.WriteFile(filepath.Join(assets, "modules", "overlay.ko"), []byte("overlay"), 0o644)
	guest := filepath.Join(t.TempDir(), "range-linux")
	os.WriteFile(guest, []byte("the guest binary, odd length"), 0o755)

	path, err := Initrd(assets, guest)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := Initrd(assets, guest); err != nil || again != path {
		t.Fatalf("a second build for the same guest = %q, %v; want the cached %q", again, err, path)
	}
	raw, _ := os.ReadFile(path)
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	archive, _ := io.ReadAll(gz)
	entries := readCPIO(t, archive)

	for name, want := range map[string]string{
		"init": string(initScript), "etc/udhcpc.sh": string(dhcpScript),
		"bin/range": "the guest binary, odd length", "bin/busybox": "busybox!",
		"lib/modules/erofs.ko": "erofs module", "lib/modules/overlay.ko": "overlay",
		"lib/modules/order": "erofs\noverlay\n", "etc/resolv.conf": "",
	} {
		got, ok := entries[name]
		if !ok || string(got.data) != want {
			t.Errorf("%s = %q (present %v), want %q", name, got.data, ok, want)
		}
	}
	if mode := entries["init"].mode; mode != 0o100755 {
		t.Errorf("init mode = %o, want an executable file", mode)
	}
	if console := entries["dev/console"]; console.mode&0o170000 != 0o020000 || console.rdev != [2]uint64{5, 1} {
		t.Errorf("dev/console = mode %o rdev %v, want the character device 5,1", console.mode, console.rdev)
	}
	if proc := entries["proc"]; proc.mode&0o170000 != 0o040000 {
		t.Errorf("proc mode = %o, want a directory", proc.mode)
	}

	// The same guest rewritten keeps its initramfs; a new guest gets a new one.
	os.WriteFile(guest, []byte("the guest binary, odd length"), 0o755)
	if again, _ := Initrd(assets, guest); again != path {
		t.Fatalf("a rewritten but identical guest built %q", again)
	}
	os.WriteFile(guest, []byte("a newer guest binary, longer than before"), 0o755)
	newer, err := Initrd(assets, guest)
	if err != nil || newer == path {
		t.Fatalf("a changed guest reused %q (%v)", newer, err)
	}
	// A few stay, so two guests in turn do not rebuild each other's.
	for i := 0; i < 5; i++ {
		os.WriteFile(guest, []byte(fmt.Sprintf("guest %d", i)), 0o755)
		if _, err := Initrd(assets, guest); err != nil {
			t.Fatal(err)
		}
	}
	if kept, _ := filepath.Glob(filepath.Join(assets, "initrd-*.gz")); len(kept) != 4 {
		t.Errorf("%d initramfs files kept, want 4", len(kept))
	}
}

func TestDiskName(t *testing.T) {
	if DiskName(0) != "/dev/vda" || DiskName(2) != "/dev/vdc" {
		t.Fatalf("DiskName = %s, %s", DiskName(0), DiskName(2))
	}
}
