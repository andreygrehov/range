package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"github.com/andreygrehov/range/internal/image"
	"github.com/andreygrehov/range/internal/vm"
)

// kvmRuntime runs each session in a VM of its own, with QEMU and KVM, the way
// Range runs one on a Mac. It needs no root: only read-write access to
// /dev/kvm and /dev/vhost-vsock, which the kvm group gives, and QEMU. The VM
// is also a security boundary, which the native runtime is not.
type kvmRuntime struct{}

func newKVM() (Runtime, bool) {
	for _, req := range (kvmRuntime{}).Requirements() {
		if !req.OK && !req.Fixable {
			return nil, false
		}
	}
	return kvmRuntime{}, true
}

func (kvmRuntime) Name() string { return "kvm" }

func (kvmRuntime) Requirements() []Requirement {
	access := func(path string) bool { return syscall.Access(path, 6 /* R_OK|W_OK */) == nil }
	_, qemuErr := exec.LookPath(vm.QEMUBinary())
	return []Requirement{
		{What: "/dev/kvm", OK: access("/dev/kvm"),
			Detail: "join the kvm group: sudo usermod -aG kvm $USER, then log in again", From: "the kernel"},
		{What: "/dev/vhost-vsock", OK: access("/dev/vhost-vsock"),
			Detail: "join the kvm group, or sudo modprobe vhost_vsock", From: "the kernel"},
		{What: "QEMU", OK: qemuErr == nil,
			Detail: vm.QEMUBinary() + " is missing: sudo apt install " + qemuPackage(), From: "your distribution"},
		{What: "Linux VM", OK: true, Fixable: true,
			Detail: "a kernel and an initramfs, downloaded once (13 MB)", From: "range, on first use"},
	}
}

func qemuPackage() string {
	if runtime.GOARCH == "arm64" {
		return "qemu-system-arm"
	}
	return "qemu-system-x86"
}

// BuildImage builds an EROFS image here: that needs no root.
func (kvmRuntime) BuildImage(_ context.Context, req image.BuildRequest) error {
	return image.BuildNative(req)
}

func (kvmRuntime) Run(ctx context.Context, opts Options, onReady func()) error {
	if len(opts.Dirs) > 0 {
		if _, err := vm.Virtiofsd(); err != nil {
			return fmt.Errorf("sharing a directory needs virtiofsd: sudo apt install virtiofsd")
		}
	}
	assets, err := vm.Assets(ctx, opts.Config.CacheDir)
	if err != nil {
		return err
	}
	// The guest runs this very binary: it is the Linux build.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	initrd, err := vm.Initrd(assets, self)
	if err != nil {
		return err
	}
	disks, err := serveDisks(opts)
	if err != nil {
		return err
	}
	cfg := vmHostConfig(opts, assets, initrd, disks, vmMemory(linuxMemory()))
	if cfg.PortBase, err = vm.RandomPortBase(); err != nil {
		return err
	}
	runDir, err := os.MkdirTemp("", "range-vm-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runDir)

	// QEMU dies with the thread that starts it; keep this goroutine on it for
	// the life of the VM, so the VM never outlives range.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	machine, err := vm.StartQEMU(cfg.Spec(), cfg.PortBase, runDir)
	if err != nil {
		return err
	}
	defer machine.Stop()
	// range cancels ctx on SIGTERM: the VM ends with it.
	go func() {
		select {
		case <-ctx.Done():
			machine.Stop()
		case <-machine.Stopped():
		}
	}()
	code, err := vm.Serve(machine, cfg, onReady)
	if ctx.Err() != nil {
		return ExitStatus(128 + int(syscall.SIGTERM))
	}
	if err != nil {
		if said := machine.Failure(); said != "" {
			err = fmt.Errorf("%w: %s", err, said)
		}
		return err
	}
	if code != 0 {
		return ExitStatus(code)
	}
	return nil
}

// linuxMemory is this machine's memory, or 0 if it cannot be read.
func linuxMemory() uint64 {
	var info syscall.Sysinfo_t
	if syscall.Sysinfo(&info) != nil {
		return 0
	}
	return uint64(info.Totalram) * uint64(info.Unit)
}
