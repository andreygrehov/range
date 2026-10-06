package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/vm"
)

// What a session in a VM of its own needs, whichever hypervisor runs it:
// Apple's Virtualization.framework on a Mac (vz_darwin.go), QEMU with KVM on
// Linux (kvm_linux.go).

// serveDisks serves the environment, then each mount, over NBD on loopback
// ports, for the VM to attach as vda, vdb, ...
func serveDisks(opts Options) ([]string, error) {
	readers := []*core.Reader{opts.Reader}
	for _, m := range opts.Mounts {
		readers = append(readers, m.Reader)
	}
	var disks []string
	for _, r := range readers {
		port, err := serveNBD(opts.Session, r)
		if err != nil {
			return nil, err
		}
		disks = append(disks, fmt.Sprintf("nbd://127.0.0.1:%d", port))
	}
	return disks, nil
}

// vmHostConfig describes the VM of a session, and what the guest runs.
func vmHostConfig(opts Options, assets, initrd string, disks []string, memory uint64) vm.HostConfig {
	terminal := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	cfg := vm.HostConfig{
		Kernel: vm.Kernel(assets), Initrd: initrd,
		CPUs: uint(min(runtime.NumCPU(), 8)), Memory: memory, Disks: disks,
		Console: filepath.Join(opts.Config.CacheDir, "vm", "console.log"),
		Guest: vm.GuestConfig{
			SessionID: opts.Session.ID, Workload: opts.Workload, Workdir: opts.Workdir,
			ShellPath: opts.ShellPath, Command: opts.Command, DefaultCommand: opts.DefaultCommand,
			Terminal: terminal, Term: os.Getenv("TERM"), Env: opts.Env,
		},
	}
	for _, m := range opts.Mounts {
		cfg.Guest.Mounts = append(cfg.Guest.Mounts, m.Target)
	}
	for i, d := range opts.Dirs {
		cfg.Guest.Shares = append(cfg.Guest.Shares, vm.Share{Tag: vm.ShareTag(i), Path: d.Path, Target: d.Target})
	}
	for _, p := range opts.Ports {
		cfg.Guest.Ports = append(cfg.Guest.Ports, p.Port)
		cfg.Publish = append(cfg.Publish, vm.Publish{HostIP: p.HostIP, HostPort: p.HostPort})
	}
	if terminal {
		cfg.Guest.Rows, cfg.Guest.Cols, _ = vm.TerminalSize(int(os.Stdout.Fd()))
	}
	return cfg
}

// vmMemory gives a VM a quarter of this machine's memory, between 2 and
// 8 GiB, in whole MiB: QEMU takes memory in MiB, and its shared memory for
// virtiofs must be exactly that size. The VM takes pages only as the guest
// touches them.
func vmMemory(total uint64) uint64 {
	if total == 0 {
		return 4 << 30
	}
	return min(max(total/4, 2<<30), 8<<30) &^ (1<<20 - 1)
}
