package vm

import (
	"bufio"
	"encoding/json"
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

	"github.com/andreygrehov/range/internal/relay"
)

// Machine is a running VM, as the host side of a session needs it: vsock
// in both directions, and whether it is still running.
type Machine interface {
	Listen(port uint32) (net.Listener, error)
	Connect(port uint32) (net.Conn, error)
	Stopped() <-chan struct{}
}

// Serve runs the host side of a session on a booted machine. It publishes
// the session's ports, tells the guest what to run, carries the standard
// streams and the terminal's size, and returns the workload's exit status.
// ready is called once the environment is up; the workload starts when it
// returns, so whatever it prints comes before the workload's first output.
func Serve(m Machine, cfg HostConfig, ready func()) (int, error) {
	for i, pub := range cfg.Publish {
		port := uint32(PortRelay + i)
		addr := net.JoinHostPort(pub.HostIP, strconv.Itoa(pub.HostPort))
		stop, err := relay.Listen(addr, func() (io.ReadWriteCloser, error) { return m.Connect(port) })
		if err != nil {
			return 1, fmt.Errorf("publish port %d: %w", cfg.Guest.Ports[i], err)
		}
		defer stop()
	}

	base := cfg.PortBase
	if base == 0 {
		base = DefaultPortBase
	}
	ports := PortsFrom(base)
	terminal := cfg.Guest.Terminal
	listeners := map[uint32]net.Listener{}
	for _, port := range []uint32{ports.Control, ports.Stdin, ports.Stdout, ports.Stderr} {
		if port == ports.Stderr && terminal {
			continue
		}
		l, err := m.Listen(port)
		if err != nil {
			return 1, err
		}
		defer l.Close()
		listeners[port] = l
	}
	accept := func(port uint32) (net.Conn, error) {
		got := make(chan net.Conn, 1)
		failed := make(chan error, 1)
		go func() {
			c, err := listeners[port].Accept()
			if err != nil {
				failed <- err
				return
			}
			got <- c
		}()
		select {
		case c := <-got:
			return c, nil
		case err := <-failed:
			return nil, err
		case <-m.Stopped():
			return nil, fmt.Errorf("the vm stopped while booting%s", consoleHint(cfg.Console))
		case <-time.After(30 * time.Second):
			return nil, fmt.Errorf("the vm did not answer%s", consoleHint(cfg.Console))
		}
	}

	control, err := accept(ports.Control)
	if err != nil {
		return 1, err
	}
	defer control.Close()
	line, _ := json.Marshal(cfg.Guest)
	if _, err := control.Write(append(line, '\n')); err != nil {
		return 1, err
	}
	stdin, err := accept(ports.Stdin)
	if err != nil {
		return 1, err
	}
	defer stdin.Close()
	var outputs sync.WaitGroup
	copyOut := func(dst io.Writer, src io.ReadCloser) {
		outputs.Add(1)
		go func() { defer outputs.Done(); io.Copy(dst, src); src.Close() }()
	}
	stdout, err := accept(ports.Stdout)
	if err != nil {
		return 1, err
	}
	copyOut(os.Stdout, stdout)
	if !terminal {
		stderr, err := accept(ports.Stderr)
		if err != nil {
			return 1, err
		}
		copyOut(os.Stderr, stderr)
	}

	// Control messages: readiness, then the exit status. The terminal is made
	// raw on that goroutine; whoever ends the session puts it back.
	status := make(chan int, 1)
	var terminalMu sync.Mutex
	restore := func() {}
	defer func() {
		terminalMu.Lock()
		restore()
		terminalMu.Unlock()
	}()
	go func() {
		lines := bufio.NewScanner(control)
		for lines.Scan() {
			fields := strings.Fields(lines.Text())
			switch {
			case len(fields) == 1 && fields[0] == "ready":
				ready()
				if terminal {
					terminalMu.Lock()
					restore = rawTerminal(int(os.Stdin.Fd()))
					terminalMu.Unlock()
					go forwardResizes(control)
				}
				fmt.Fprintln(control, "go")
				if !terminal {
					go forwardInterrupts(control)
				}
				go func() {
					io.Copy(stdin, os.Stdin)
					// The guest only reads this stream, so closing it is how
					// its standard input ends: vz has no half-close.
					stdin.Close()
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
	case <-m.Stopped():
		err = fmt.Errorf("the vm stopped before the workload finished%s", consoleHint(cfg.Console))
	}
	// The guest closes its output streams before it reports the status.
	drained := make(chan struct{})
	go func() { outputs.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	}
	return code, err
}

func consoleHint(path string) string {
	if path == "" {
		return ""
	}
	return "; its console output is in " + path
}

// forwardInterrupts passes Ctrl+C on to the workload. Without a raw terminal
// it arrives here as SIGINT, not as a keystroke for the guest.
func forwardInterrupts(control io.Writer) {
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	for range interrupts {
		fmt.Fprintf(control, "signal %d\n", syscall.SIGINT)
	}
}

// forwardResizes tells the guest the terminal's size whenever it changes.
func forwardResizes(control io.Writer) {
	changes := make(chan os.Signal, 1)
	signal.Notify(changes, syscall.SIGWINCH)
	for range changes {
		if rows, cols, ok := TerminalSize(int(os.Stdin.Fd())); ok {
			fmt.Fprintf(control, "resize %d %d\n", rows, cols)
		}
	}
}
