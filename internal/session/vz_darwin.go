//go:build darwin && cgo

package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/image"
	"github.com/andreygrehov/range/internal/vm"
)

// vzRuntime runs each session in a VM of its own, booted with Apple's
// Virtualization.framework in a fraction of a second. There is nothing to
// install: the VM is a kernel and an initramfs Range downloads once, and the
// environment is a disk Range serves over NBD from this Mac, with the cache
// and the profiles it keeps here.
type vzRuntime struct{}

func newVZ() (Runtime, bool) {
	if os.Getenv("RANGE_RUNTIME") == "lima" || runtime.GOARCH != "arm64" || macOSMajor() < 14 {
		return nil, false
	}
	return vzRuntime{}, true
}

func (vzRuntime) Name() string { return "vz" }

// macOSMajor is the major macOS version, or 0 if it cannot be read.
func macOSMajor() int {
	version, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return 0
	}
	major := 0
	fmt.Sscanf(version, "%d", &major)
	return major
}

func (vzRuntime) Requirements() []Requirement {
	reqs := []Requirement{{What: "macOS 14+", OK: macOSMajor() >= 14,
		Detail: "Virtualization.framework attaches network disks from macOS 14", From: "Apple"}}
	_, err := exec.LookPath("codesign")
	reqs = append(reqs, Requirement{What: "codesign", OK: err == nil,
		Detail: "/usr/bin/codesign ships with macOS; restore it, or set RANGE_RUNTIME=lima", From: "macOS"})
	if err := guestBinaryAvailable(); err != nil {
		reqs = append(reqs, Requirement{What: "guest binary", OK: false, Detail: err.Error(), From: "range"})
	}
	reqs = append(reqs, Requirement{What: "Linux VM", OK: true, Fixable: true,
		Detail: "a kernel and an initramfs, downloaded once (13 MB)", From: "range, on first use"})
	return reqs
}

// BuildImage still runs in the Lima VM: building is rare, and needs a disk.
func (vzRuntime) BuildImage(ctx context.Context, req image.BuildRequest) error {
	return lima{instance: limaInstance}.BuildImage(ctx, req)
}

func (vzRuntime) Run(ctx context.Context, opts Options, onReady func()) error {
	cacheDir := opts.Config.CacheDir
	assets, err := vm.Assets(ctx, cacheDir)
	if err != nil {
		return err
	}
	guest, err := guestBinarySource()
	if err != nil {
		return err
	}
	initrd, err := vm.Initrd(assets, guest)
	if err != nil {
		return err
	}
	host, err := signedSelf(cacheDir)
	if err != nil {
		return err
	}

	disks := []string{}
	for _, r := range append([]*core.Reader{opts.Reader}, mountReaders(opts.Mounts)...) {
		port, err := serveNBD(opts.Session, r)
		if err != nil {
			return err
		}
		disks = append(disks, fmt.Sprintf("nbd://127.0.0.1:%d", port))
	}
	terminal := isTerminal(os.Stdin) && isTerminal(os.Stdout)
	cfg := vm.HostConfig{
		Kernel: filepath.Join(assets, "Image"), Initrd: initrd,
		CPUs: uint(min(runtime.NumCPU(), 8)), Memory: vmMemory(), Disks: disks,
		Console: filepath.Join(cacheDir, "vm", "console.log"),
		Guest: vm.GuestConfig{
			SessionID: opts.Session.ID, Workload: opts.Workload, Workdir: opts.Workdir,
			ShellPath: opts.ShellPath, Command: opts.Command, DefaultCommand: opts.DefaultCommand,
			Terminal: terminal, Term: os.Getenv("TERM"),
		},
	}
	for _, m := range opts.Mounts {
		cfg.Guest.Mounts = append(cfg.Guest.Mounts, m.Target)
	}
	for i, d := range opts.Dirs {
		cfg.Guest.Shares = append(cfg.Guest.Shares, vm.Share{Tag: vm.ShareTag(i), Path: d.Path, Target: d.Target})
	}
	if terminal {
		cfg.Guest.Rows, cfg.Guest.Cols = windowSize(int(os.Stdout.Fd()))
	}

	configRead, configWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	goRead, goWrite, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, host, VMHostCommand)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{configRead, readyWrite, goRead}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the vm: %w", err)
	}
	configRead.Close()
	readyWrite.Close()
	goRead.Close()
	json.NewEncoder(configWrite).Encode(cfg)
	configWrite.Close()
	go func() {
		defer goWrite.Close()
		if _, err := io.ReadFull(readyRead, make([]byte, 1)); err != nil {
			return
		}
		onReady()
		goWrite.Write([]byte{'G'})
	}()
	return exitStatusOf(cmd.Wait())
}

func mountReaders(mounts []Mount) []*core.Reader {
	readers := make([]*core.Reader, len(mounts))
	for i, m := range mounts {
		readers[i] = m.Reader
	}
	return readers
}

// vmMemory gives the VM a quarter of this Mac's memory, between 2 and 8 GiB.
// The VM takes pages only as the guest touches them.
func vmMemory() uint64 {
	raw, err := syscall.Sysctl("hw.memsize")
	if err != nil || len(raw) > 8 {
		return 4 << 30
	}
	// Sysctl hands back the raw value, with a trailing zero byte cut off.
	var b [8]byte
	copy(b[:], raw)
	total := binary.LittleEndian.Uint64(b[:])
	return min(max(total/4, 2<<30), 8<<30)
}

func windowSize(fd int) (rows, cols uint16) {
	var size [4]uint16
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	return size[0], size[1]
}

// entitlements lets a binary use Virtualization.framework. An ad-hoc
// signature carrying it is enough: no developer account is involved.
const entitlements = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>com.apple.security.virtualization</key><true/></dict></plist>
`

// signedSelf returns a copy of this binary signed with the virtualization
// entitlement, made once for each build. A binary from "go build" or
// "go run" is not signed that way, and macOS refuses it a VM. A copy is
// known by the size and time of the binary it was made from; older copies
// are removed.
func signedSelf(cacheDir string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	info, err := os.Stat(self)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cacheDir, "vm", "host")
	path := filepath.Join(dir, fmt.Sprintf("range-%x-%x", info.Size(), info.ModTime().UnixNano()))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(self)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".sign-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	tmp.Close()
	os.Chmod(tmp.Name(), 0o755)
	plist := filepath.Join(dir, "entitlements.plist")
	if err := os.WriteFile(plist, []byte(entitlements), 0o644); err != nil {
		return "", err
	}
	sign := exec.Command("codesign", "--force", "--sign", "-", "--entitlements", plist, tmp.Name())
	if out, err := sign.CombinedOutput(); err != nil {
		return "", fmt.Errorf("sign range for Virtualization.framework: %s", strings.TrimSpace(string(out)))
	}
	old, _ := filepath.Glob(filepath.Join(dir, "range-*"))
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	for _, stale := range old {
		os.Remove(stale)
	}
	return path, nil
}
