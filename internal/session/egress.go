package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/andreygrehov/range/internal/egress"
)

// A native session without a network has a network namespace of its own,
// with lo and nothing else. With --allow-host, its proxy serves on a Unix
// socket in the session's directory, and a relay that joins only the
// session's network namespace offers it at 127.0.0.1:3128 there. The relay
// is not in the session's PID namespace: the workload cannot see or end it.

// EgressRelayCommand is the relay, as range runs it.
const EgressRelayCommand = "__egress-relay"

// serveEgress serves the session's proxy, and returns what starts its relay
// once the session's namespaces exist.
func serveEgress(sess *Session, opts Options) (func(pid int) error, error) {
	sock := filepath.Join(sess.dir, "egress.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("serve the session's way out: %w", err)
	}
	sess.Push(l.Close)
	proxy := &egress.Proxy{Policy: *opts.Egress, Log: opts.EgressLog}
	go proxy.Serve(l)
	return func(pid int) error {
		if err := waitForOwnNetwork(pid); err != nil {
			return err
		}
		self, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command("nsenter", "--target", strconv.Itoa(pid), "--net", "--", self, EgressRelayCommand, sock)
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		cmd.SysProcAttr = dieWithParent(nil)
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start the session's way out: %w", err)
		}
		sess.Push(func() error { cmd.Process.Kill(); cmd.Wait(); return nil })
		ready := make(chan error, 1)
		go func() {
			line, err := bufio.NewReader(out).ReadString('\n')
			if err == nil && line != "ready\n" {
				err = errors.New(line)
			}
			ready <- err
		}()
		select {
		case err := <-ready:
			if err != nil {
				return fmt.Errorf("start the session's way out: %w", err)
			}
		case <-time.After(5 * time.Second):
			return errors.New("the session's way out did not start")
		}
		return nil
	}, nil
}

// RunEgressRelay is the relay: it offers the proxy on sock at 127.0.0.1:3128
// in this network namespace, and says "ready" when it does.
func RunEgressRelay(args []string) error {
	if len(args) != 1 {
		return errors.New(EgressRelayCommand + " is started by range, not by hand")
	}
	sock := args[0]
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", egress.Port))
	if err != nil {
		return err
	}
	fmt.Println("ready")
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer conn.Close()
			proxy, err := net.Dial("unix", sock)
			if err != nil {
				return
			}
			defer proxy.Close()
			go func() { io.Copy(proxy, conn); proxy.(*net.UnixConn).CloseWrite() }()
			io.Copy(conn, proxy)
		}()
	}
}
