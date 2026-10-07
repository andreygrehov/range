package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/andreygrehov/range/internal/relay"
	"github.com/andreygrehov/range/internal/session"
	"github.com/andreygrehov/range/internal/vm"
)

// commandVMGuest is the first process of the VM Range boots on a Mac, run by
// its init script. It asks the host what to run, wires the workload's
// standard streams to the host, and then mounts and runs exactly as a native
// Linux session does, from /dev/vda. When the workload ends it reports the
// status and powers the VM off.
func commandVMGuest(_ []string) error {
	code, err := runVMGuest()
	if err != nil {
		fmt.Fprintf(os.Stderr, "range: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	syscall.Sync()
	// Give the host a moment to drain the streams before the VM goes away.
	time.Sleep(20 * time.Millisecond)
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
	os.Exit(code)
	return nil
}

func runVMGuest() (int, error) {
	cmdline, _ := os.ReadFile("/proc/cmdline")
	ports := vm.PortsFrom(vm.PortBase(string(cmdline)))
	control, err := vm.DialHost(ports.Control, 10*time.Second)
	if err != nil {
		return 1, err
	}
	defer control.Close()
	lines := bufio.NewReader(control)
	first, err := lines.ReadBytes('\n')
	if err != nil {
		return 1, fmt.Errorf("read the session from the host: %w", err)
	}
	var cfg vm.GuestConfig
	if err := json.Unmarshal(first, &cfg); err != nil {
		return 1, fmt.Errorf("decode the session from the host: %w", err)
	}
	if cfg.Term != "" {
		os.Setenv("TERM", cfg.Term)
	}

	streams, err := connectStreams(cfg, ports)
	if err != nil {
		return 1, err
	}

	// The host answers "go" once its banner is printed, and later sends the
	// terminal's new size whenever it changes.
	proceed := make(chan struct{})
	go func() {
		var once sync.Once
		for {
			line, err := lines.ReadString('\n')
			if err != nil {
				once.Do(func() { close(proceed) })
				return
			}
			fields := strings.Fields(line)
			switch {
			case len(fields) == 1 && fields[0] == "go":
				once.Do(func() { close(proceed) })
			case len(fields) == 2 && fields[0] == "signal":
				// Every process but this one, as the terminal signals every
				// process in its foreground group: the workload, and unshare.
				if sig, err := strconv.Atoi(fields[1]); err == nil {
					syscall.Kill(-1, syscall.Signal(sig))
				}
			case len(fields) == 3 && fields[0] == "resize" && streams.terminal != nil:
				rows, _ := strconv.Atoi(fields[1])
				cols, _ := strconv.Atoi(fields[2])
				setWindowSize(streams.terminal, uint16(rows), uint16(cols))
			}
		}
	}()

	waitForResolver(3 * time.Second)
	sess := &session.Session{ID: cfg.SessionID, State: session.StateAttaching, StartedAt: time.Now()}
	if sess.ID == "" {
		sess.ID = session.NewID()
	}
	if err := session.Prepare(sess, "", "", false); err != nil {
		return 1, err
	}
	mounts := make([]session.Mount, len(cfg.Mounts))
	for i, target := range cfg.Mounts {
		mounts[i] = session.Mount{Target: target, Device: vm.DiskName(i + 1)}
	}
	// Each directory of the Mac arrives as a virtiofs share, mounted here
	// first and then shown in the environment like a directory on Linux.
	var dirs []session.Dir
	for i, share := range cfg.Shares {
		at := fmt.Sprintf("/run/range-share/%d", i)
		if err := os.MkdirAll(at, 0o755); err != nil {
			return 1, err
		}
		if err := syscall.Mount(share.Tag, at, "virtiofs", 0, ""); err != nil {
			return 1, fmt.Errorf("mount the shared directory for %s: %w", share.Target, err)
		}
		dirs = append(dirs, session.Dir{Path: at, Target: share.Target})
	}
	for i, port := range cfg.Ports {
		if err := relayPort(uint32(vm.PortRelay+i), port); err != nil {
			return 1, err
		}
	}
	opts := session.Options{
		Session: sess, Workload: cfg.Workload, Workdir: cfg.Workdir, ShellPath: cfg.ShellPath,
		Command: cfg.Command, Mounts: mounts, Dirs: dirs, Env: cfg.Env, DefaultCommand: cfg.DefaultCommand,
		ControllingTerminal: cfg.Terminal, Tools: "/bin/busybox", LoadFilesystem: vm.LoadFilesystem,
	}
	runErr := session.MountAndRun(context.Background(), opts, vm.DiskName(0), func() {
		fmt.Fprintln(control, "ready")
		<-proceed
	})
	cleanupErr := sess.Cleanup()

	// Say what failed while the host still reads standard error: once the
	// streams are finished, nothing printed here reaches it.
	code := 0
	var status session.ExitStatus
	switch {
	case errors.As(runErr, &status):
		code = int(status)
	case runErr != nil:
		code = 1
		fmt.Fprintf(os.Stderr, "range: %v\n", runErr)
	case cleanupErr != nil:
		fmt.Fprintf(os.Stderr, "range: %v\n", cleanupErr)
	}
	streams.finish()
	fmt.Fprintf(control, "exit %d\n", code)
	return code, nil
}

// relayPort takes the host's connections for a published port and relays
// each to that port here. The environment shares this VM's network, so a
// server listening on 127.0.0.1 inside is reached too, not only one on
// 0.0.0.0.
func relayPort(vsockPort uint32, port int) error {
	listener, err := vm.ListenHost(vsockPort)
	if err != nil {
		return fmt.Errorf("publish port %d: %w", port, err)
	}
	target := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				inside, err := net.Dial("tcp", target)
				if err != nil {
					conn.Close()
					return
				}
				relay.Copy(conn, inside)
			}()
		}
	}()
	return nil
}

// waitForResolver waits until DHCP has named a DNS server: a workload that
// starts by downloading something must not find the network half up. The
// lease usually lands well within the boot. Without one, the workload runs
// anyway, offline.
func waitForResolver(limit time.Duration) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile("/etc/resolv.conf"); err == nil && strings.Contains(string(data), "nameserver") {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Fprintln(os.Stderr, "range: the VM got no network address; running offline")
}

// guestStreams connects standard input, output and error of this process,
// which the workload inherits, to the host.
type guestStreams struct {
	terminal *os.File // the pty master, with a terminal
	copies   sync.WaitGroup
	outputs  []*os.File // vsock connections the host reads to EOF
}

func connectStreams(cfg vm.GuestConfig, ports vm.Ports) (*guestStreams, error) {
	g := &guestStreams{}
	stdin, err := vm.DialHost(ports.Stdin, 5*time.Second)
	if err != nil {
		return nil, err
	}
	stdout, err := vm.DialHost(ports.Stdout, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if cfg.Terminal {
		master, slave, err := openPTY()
		if err != nil {
			return nil, err
		}
		setWindowSize(master, cfg.Rows, cfg.Cols)
		for fd := 0; fd <= 2; fd++ {
			if err := syscall.Dup3(int(slave.Fd()), fd, 0); err != nil {
				return nil, err
			}
		}
		slave.Close()
		g.terminal = master
		g.pump(master, stdin, nil)
		g.pump(stdout, master, stdout)
		g.outputs = []*os.File{stdout}
		return g, nil
	}
	stderr, err := vm.DialHost(ports.Stderr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	for fd, f := range []*os.File{inRead, outWrite, errWrite} {
		if err := syscall.Dup3(int(f.Fd()), fd, 0); err != nil {
			return nil, err
		}
		f.Close()
	}
	// Standard input ends when the host's does: closing the pipe gives the
	// workload EOF.
	go func() {
		io.Copy(inWrite, stdin)
		inWrite.Close()
	}()
	g.pump(stdout, outRead, stdout)
	g.pump(stderr, errRead, stderr)
	g.outputs = []*os.File{stdout, stderr}
	return g, nil
}

// pump copies src to dst. When src ends, done's sending half is closed so the
// host sees the end of the stream.
func (g *guestStreams) pump(dst io.Writer, src io.Reader, done *os.File) {
	g.copies.Add(1)
	go func() {
		defer g.copies.Done()
		io.Copy(dst, src)
		if done != nil {
			vm.CloseWrite(done)
		}
	}()
}

// finish closes this process's own copies of the output streams, so the
// copies to the host reach EOF once the workload's copies are gone too, and
// waits for them a short while.
func (g *guestStreams) finish() {
	for fd := 1; fd <= 2; fd++ {
		syscall.Close(fd)
	}
	if g.terminal != nil {
		// A pty master reads EIO, not EOF, once no one holds the slave.
		syscall.Close(0)
	}
	done := make(chan struct{})
	go func() { g.copies.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		for _, f := range g.outputs {
			vm.CloseWrite(f)
		}
	}
}

func openPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if err := ioctl(master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}
	var n uint32
	if err := ioctl(master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("pty number: %w", err)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

func setWindowSize(f *os.File, rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	size := [4]uint16{rows, cols, 0, 0}
	ioctl(f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
}

func ioctl(fd, request, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg); errno != 0 {
		return errno
	}
	return nil
}
