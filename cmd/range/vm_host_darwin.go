//go:build darwin && cgo

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/andreygrehov/range/internal/session"
	"github.com/andreygrehov/range/internal/vm"
)

// commandVMHost runs the VM of one session. range starts it from a copy of
// itself signed for Virtualization.framework, with the terminal inherited:
// it boots the VM, tells the guest what to run, carries the standard streams
// and the window size, and exits with the workload's status.
func commandVMHost(_ []string) error {
	configFile, ready, proceed := os.NewFile(3, "config"), os.NewFile(4, "ready"), os.NewFile(5, "go")
	if configFile == nil || ready == nil || proceed == nil {
		return errors.New(session.VMHostCommand + " is started by range, not by hand")
	}
	var cfg vm.HostConfig
	if err := json.NewDecoder(configFile).Decode(&cfg); err != nil {
		return fmt.Errorf("read the vm configuration: %w", err)
	}
	configFile.Close()

	var console *os.File
	if cfg.Console != "" {
		console, _ = os.Create(cfg.Console)
	}
	machine, err := vm.Start(vm.Spec{
		Kernel: cfg.Kernel, Initrd: cfg.Initrd, CPUs: cfg.CPUs, Memory: cfg.Memory,
		Disks: cfg.Disks, Console: console, Shares: cfg.Guest.Shares,
	})
	if err != nil {
		return err
	}
	defer machine.Close()
	defer machine.Stop()
	for i, pub := range cfg.Publish {
		relay := uint32(vm.PortRelay + i)
		port := session.Port{HostIP: pub.HostIP, HostPort: pub.HostPort, Port: cfg.Guest.Ports[i]}
		stop, err := session.Publish(port, func() (io.ReadWriteCloser, error) { return machine.Connect(relay) })
		if err != nil {
			return err
		}
		defer stop()
	}

	listen := func(port uint32) (net.Listener, error) { return machine.Listen(port) }
	controlL, err := listen(vm.PortControl)
	if err != nil {
		return err
	}
	stdinL, err := listen(vm.PortStdin)
	if err != nil {
		return err
	}
	stdoutL, err := listen(vm.PortStdout)
	if err != nil {
		return err
	}
	var stderrL net.Listener
	if !cfg.Guest.Terminal {
		if stderrL, err = listen(vm.PortStderr); err != nil {
			return err
		}
	}

	accept := func(l net.Listener) (net.Conn, error) {
		type result struct {
			conn net.Conn
			err  error
		}
		got := make(chan result, 1)
		go func() { c, err := l.Accept(); got <- result{c, err} }()
		select {
		case r := <-got:
			return r.conn, r.err
		case <-machine.Stopped():
			return nil, fmt.Errorf("the vm stopped while booting%s", consoleHint(cfg.Console))
		case <-time.After(30 * time.Second):
			return nil, fmt.Errorf("the vm did not answer%s", consoleHint(cfg.Console))
		}
	}
	control, err := accept(controlL)
	if err != nil {
		return err
	}
	line, _ := json.Marshal(cfg.Guest)
	if _, err := control.Write(append(line, '\n')); err != nil {
		return err
	}
	stdinConn, err := accept(stdinL)
	if err != nil {
		return err
	}
	stdoutConn, err := accept(stdoutL)
	if err != nil {
		return err
	}
	var outputs sync.WaitGroup
	copyOut := func(dst io.Writer, src io.Reader) {
		outputs.Add(1)
		go func() { defer outputs.Done(); io.Copy(dst, src) }()
	}
	copyOut(os.Stdout, stdoutConn)
	if stderrL != nil {
		stderrConn, err := accept(stderrL)
		if err != nil {
			return err
		}
		copyOut(os.Stderr, stderrConn)
	}

	// Control messages: readiness, then the exit status.
	status := make(chan int, 1)
	// The terminal is made raw on the control goroutine; whoever ends the
	// session puts it back.
	var terminalMu sync.Mutex
	restore := func() {}
	go func() {
		lines := bufio.NewScanner(control)
		for lines.Scan() {
			fields := strings.Fields(lines.Text())
			switch {
			case len(fields) == 1 && fields[0] == "ready":
				ready.Write([]byte{'R'})
				io.ReadFull(proceed, make([]byte, 1))
				if cfg.Guest.Terminal {
					terminalMu.Lock()
					restore = rawTerminal(int(os.Stdin.Fd()))
					terminalMu.Unlock()
					go forwardResizes(control)
				}
				fmt.Fprintln(control, "go")
				go func() {
					io.Copy(stdinConn, os.Stdin)
					// vsock has no half-close here; the guest only reads this
					// stream, so closing it is how its standard input ends.
					stdinConn.Close()
				}()
			case len(fields) == 2 && fields[0] == "exit":
				code, _ := strconv.Atoi(fields[1])
				status <- code
				return
			}
		}
	}()

	code := 1
	select {
	case code = <-status:
	case <-machine.Stopped():
		fmt.Fprintf(os.Stderr, "range: the vm stopped before the workload finished%s\n", consoleHint(cfg.Console))
	case <-parentGone():
		return errors.New("range ended; stopping its vm")
	}
	// The guest closes its output streams before it reports the status.
	drained := make(chan struct{})
	go func() { outputs.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	}
	terminalMu.Lock()
	restore()
	terminalMu.Unlock()
	select {
	case <-machine.Stopped():
	case <-time.After(2 * time.Second):
	}
	if code != 0 {
		return session.ExitStatus(code)
	}
	return nil
}

// parentGone is closed if the range that started this process ends without
// stopping it, killed outright, so its VM does not run on with no one to
// stop it.
func parentGone() <-chan struct{} {
	gone := make(chan struct{})
	parent := os.Getppid()
	go func() {
		for os.Getppid() == parent {
			time.Sleep(time.Second)
		}
		close(gone)
	}()
	return gone
}

func consoleHint(path string) string {
	if path == "" {
		return ""
	}
	return "; its console output is in " + path
}

// forwardResizes tells the guest the terminal's size whenever it changes.
func forwardResizes(control io.Writer) {
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	for range changes {
		if rows, cols, ok := terminalSize(int(os.Stdin.Fd())); ok {
			fmt.Fprintf(control, "resize %d %d\n", rows, cols)
		}
	}
}

func terminalSize(fd int) (rows, cols uint16, ok bool) {
	var size [4]uint16
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	return size[0], size[1], errno == 0
}

// rawTerminal puts the terminal in raw mode, as ssh does for a remote pty,
// and returns how to put it back.
func rawTerminal(fd int) func() {
	var saved syscall.Termios
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGETA, uintptr(unsafe.Pointer(&saved))); errno != 0 {
		return func() {}
	}
	raw := saved
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(&raw)))
	return func() {
		syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCSETA, uintptr(unsafe.Pointer(&saved)))
	}
}
