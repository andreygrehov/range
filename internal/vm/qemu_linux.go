package vm

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// QEMU is a VM run by QEMU with KVM. It needs read-write access to /dev/kvm
// and /dev/vhost-vsock, which membership of the kvm group gives, and no
// root: the network is QEMU's own, in user space.
type QEMU struct {
	cid     uint32
	cmd     *exec.Cmd
	stderr  bytes.Buffer
	helpers []*exec.Cmd // virtiofsd, one for each share
	stopped chan struct{}
	stop    sync.Once
}

// QEMUBinary is the QEMU system emulator for this machine's architecture.
func QEMUBinary() string {
	if runtime.GOARCH == "arm64" {
		return "qemu-system-aarch64"
	}
	return "qemu-system-x86_64"
}

// Virtiofsd finds virtiofsd, which shares a directory with QEMU. Ubuntu and
// Debian install it outside PATH.
func Virtiofsd() (string, error) {
	if path, err := exec.LookPath("virtiofsd"); err == nil {
		return path, nil
	}
	for _, path := range []string{"/usr/libexec/virtiofsd", "/usr/lib/qemu/virtiofsd"} {
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", errors.New("virtiofsd is not installed")
}

// StartQEMU boots a VM from spec. Its vsock ports for the session start at
// portBase, and runDir holds its sockets. QEMU and virtiofsd die with the
// thread that starts them: the caller keeps that goroutine on its thread
// until the VM stops, so they never outlive range.
//
// The guest's vsock address is random among four billion: another VM on the
// host holding it is too unlikely to wait for at every start. QEMU refuses a
// taken one and stops, and Failure then says so.
func StartQEMU(spec Spec, portBase uint32, runDir string) (*QEMU, error) {
	cid, err := randomCID()
	if err != nil {
		return nil, err
	}
	m := &QEMU{cid: cid, stopped: make(chan struct{})}
	var sockets []string
	if len(spec.Shares) > 0 {
		virtiofsd, err := Virtiofsd()
		if err != nil {
			return nil, err
		}
		for i, share := range spec.Shares {
			socket := filepath.Join(runDir, fmt.Sprintf("fs%d.sock", i))
			helper := exec.Command(virtiofsd, "--socket-path="+socket, "--shared-dir="+share.Path, "--sandbox=none")
			helper.SysProcAttr = detached()
			if err := helper.Start(); err != nil {
				m.killHelpers()
				return nil, fmt.Errorf("start virtiofsd for %s: %w", share.Path, err)
			}
			m.helpers = append(m.helpers, helper)
			if err := waitForFile(socket, 5*time.Second); err != nil {
				m.killHelpers()
				return nil, fmt.Errorf("virtiofsd for %s: %w", share.Path, err)
			}
			sockets = append(sockets, socket)
		}
	}
	args := qemuArgs(spec, runtime.GOARCH, cid, portBase, sockets)

	m.cmd = exec.Command(QEMUBinary(), args...)
	m.cmd.Stderr = &m.stderr
	m.cmd.SysProcAttr = detached()
	if err := m.cmd.Start(); err != nil {
		m.killHelpers()
		return nil, fmt.Errorf("start %s: %w", QEMUBinary(), err)
	}
	go func() {
		m.cmd.Wait()
		m.killHelpers()
		close(m.stopped)
	}()
	return m, nil
}

// qemuArgs is QEMU's command line for spec on arch: the kernel and initramfs,
// the console, the network, vsock, the disks served over NBD, and a
// virtiofsd socket for each share.
func qemuArgs(spec Spec, arch string, cid, portBase uint32, shareSockets []string) []string {
	console := spec.Console
	if console == "" {
		console = os.DevNull
	}
	machine := "virt"
	if arch != "arm64" {
		machine = "q35"
	}
	args := []string{
		"-machine", machine, "-accel", "kvm", "-cpu", "host",
		"-m", strconv.FormatUint(spec.Memory>>20, 10), "-smp", strconv.FormatUint(uint64(spec.CPUs), 10),
		"-nodefaults", "-no-user-config", "-display", "none", "-serial", "none", "-monitor", "none",
		"-no-reboot",
		"-kernel", spec.Kernel, "-initrd", spec.Initrd,
		"-append", fmt.Sprintf("console=hvc0 quiet loglevel=3 rdinit=/init range.port=%d", portBase),
		"-device", "virtio-serial-pci", "-chardev", "file,id=console,path=" + console,
		"-device", "virtconsole,chardev=console",
		"-device", "virtio-rng-pci",
		"-netdev", "user,id=net0", "-device", "virtio-net-pci,netdev=net0",
		"-device", fmt.Sprintf("vhost-vsock-pci,guest-cid=%d", cid),
	}
	for i, url := range spec.Disks {
		args = append(args,
			"-drive", fmt.Sprintf("file=%s,format=raw,if=none,id=disk%d,readonly=on", url, i),
			"-device", fmt.Sprintf("virtio-blk-pci,drive=disk%d", i))
	}
	if len(shareSockets) > 0 {
		// vhost-user devices need the guest's memory shared with virtiofsd.
		args = append(args,
			"-object", fmt.Sprintf("memory-backend-memfd,id=mem,size=%d,share=on", spec.Memory),
			"-machine", "memory-backend=mem")
		for i, socket := range shareSockets {
			args = append(args,
				"-chardev", fmt.Sprintf("socket,id=fs%d,path=%s", i, socket),
				"-device", fmt.Sprintf("vhost-user-fs-pci,chardev=fs%d,tag=%s", i, spec.Shares[i].Tag))
		}
	}
	return args
}

// Failure is what QEMU said before it stopped, once it has stopped.
func (m *QEMU) Failure() string {
	select {
	case <-m.stopped:
		return strings.TrimSpace(m.stderr.String())
	default:
		return ""
	}
}

// Listen accepts this VM's connections to a vsock port of the host.
func (m *QEMU) Listen(port uint32) (net.Listener, error) {
	l, err := ListenHost(port)
	if err != nil {
		return nil, err
	}
	return vmListener{l: l, cid: m.cid}, nil
}

// Connect opens a connection to the guest on a vsock port.
func (m *QEMU) Connect(port uint32) (net.Conn, error) {
	f, err := dial(m.cid, port)
	if err != nil {
		return nil, err
	}
	return vsockConn{f}, nil
}

// Stopped is closed once the VM has stopped.
func (m *QEMU) Stopped() <-chan struct{} { return m.stopped }

// Stop ends the VM at once and waits for it.
func (m *QEMU) Stop() {
	m.stop.Do(func() {
		if m.cmd.Process != nil {
			m.cmd.Process.Kill()
		}
	})
	select {
	case <-m.stopped:
	case <-time.After(5 * time.Second):
	}
}

func (m *QEMU) killHelpers() {
	for _, helper := range m.helpers {
		helper.Process.Kill()
		helper.Wait()
	}
}

// detached keeps a helper process out of the terminal's process group, so
// Ctrl+C reaches the workload through range and does not end the VM, and
// kills it when range ends.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
}

// randomCID picks a guest CID: 0 to 2 are reserved, and 0xFFFFFFFF means any.
func randomCID() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return 3 + binary.LittleEndian.Uint32(b[:])%(0xFFFFFFFF-4), nil
}

// RandomPortBase picks the first of a session's vsock ports on the host,
// apart from DefaultPortBase and the relay ports.
func RandomPortBase() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return 0x10000 + binary.LittleEndian.Uint32(b[:])%0x7FFF0000&^3, nil
}

func waitForFile(path string, limit time.Duration) error {
	for deadline := time.Now().Add(limit); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", path)
		}
	}
}
