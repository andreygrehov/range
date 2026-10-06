package vm

import (
	"strings"
	"testing"
)

func TestQEMUArgs(t *testing.T) {
	spec := Spec{Kernel: "/k", Initrd: "/i", CPUs: 4, Memory: 3 << 30, Console: "/c.log",
		Disks:  []string{"nbd://127.0.0.1:1000", "nbd://127.0.0.1:1001"},
		Shares: []Share{{Tag: "range0", Path: "/home/me/p", Target: "/work"}}}
	args := strings.Join(qemuArgs(spec, "arm64", "/qboot.rom", 77, 70000, []string{"/run/fs0.sock"}), " ")
	for _, want := range []string{
		"-machine virt -accel kvm -cpu host", "-m 3072 -smp 4", "-kernel /k -initrd /i",
		"range.port=70000", "range.net=10.0.2.15/24,10.0.2.2,10.0.2.3", "path=/c.log", "vhost-vsock-pci,guest-cid=77",
		"file=nbd://127.0.0.1:1000,format=raw,if=none,id=disk0,readonly=on",
		"file=nbd://127.0.0.1:1001,format=raw,if=none,id=disk1,readonly=on",
		"memory-backend-memfd,id=mem,size=3221225472,share=on",
		"socket,id=fs0,path=/run/fs0.sock", "vhost-user-fs-pci,chardev=fs0,tag=range0",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("arm64 args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "-bios") {
		t.Errorf("arm64 args name a firmware:\n%s", args)
	}
	x86 := strings.Join(qemuArgs(Spec{Kernel: "/k", Initrd: "/i", CPUs: 2, Memory: 2 << 30}, "amd64", "/qboot.rom", 5, 1, nil), " ")
	if !strings.Contains(x86, "-machine q35") || strings.Contains(x86, "memory-backend") ||
		!strings.Contains(x86, "path=/dev/null") || !strings.Contains(x86, "-bios /qboot.rom") {
		t.Errorf("amd64 args without shares or a console are wrong:\n%s", x86)
	}
}

func TestRandomCIDAndPortBase(t *testing.T) {
	for i := 0; i < 1000; i++ {
		cid, err := randomCID()
		if err != nil || cid < 3 || cid == 0xFFFFFFFF {
			t.Fatalf("randomCID() = %d, %v", cid, err)
		}
		base, err := RandomPortBase()
		if err != nil || base < 0x10000 || base%4 != 0 || base > 0xFFFFFFFF-4 {
			t.Fatalf("RandomPortBase() = %d, %v", base, err)
		}
	}
}
