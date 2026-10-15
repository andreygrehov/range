package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/nbd"
	"github.com/andreygrehov/range/internal/session"
	"github.com/andreygrehov/range/internal/vm"
)

// guestCommands is the guest's side of commands from other images: it asks
// the host, which has the catalog and the credentials, for the image of a
// command and to serve it, and attaches what the host serves as a block
// device of its own.
type guestCommands struct {
	port uint32 // the host's commands port

	mu     sync.Mutex // one image at a time takes a free nbd device
	loaded bool
}

func (g *guestCommands) ask(req vm.CommandRequest) (vm.CommandReply, error) {
	conn, err := vm.DialHost(g.port, 10*time.Second)
	if err != nil {
		return vm.CommandReply{}, err
	}
	defer conn.Close()
	var reply vm.CommandReply
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return reply, err
	}
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return reply, err
	}
	if reply.Error != "" {
		return reply, errors.New(reply.Error)
	}
	return reply, nil
}

func (g *guestCommands) Names(context.Context) ([]string, error) {
	reply, err := g.ask(vm.CommandRequest{Op: "names"})
	return reply.Names, err
}

func (g *guestCommands) Find(_ context.Context, command, version string) (string, string, error) {
	reply, err := g.ask(vm.CommandRequest{Op: "find", Command: command, Version: version})
	return reply.Image, reply.Program, err
}

// Open has the host serve image, and attaches it to a free nbd device.
func (g *guestCommands) Open(_ context.Context, image string) (string, func() error, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.loaded {
		if err := vm.LoadModule("nbd", "nbds_max=16"); err != nil {
			return "", nil, err
		}
		if err := waitFor("/sys/block/nbd0", 5*time.Second); err != nil {
			return "", nil, fmt.Errorf("the nbd module: %w", err)
		}
		g.loaded = true
	}
	reply, err := g.ask(vm.CommandRequest{Op: "open", Image: image})
	if err != nil {
		return "", nil, err
	}
	device, err := session.AllocateNBDDevice("/sys/block", "/dev")
	if err != nil {
		return "", nil, err
	}
	conn, err := vm.DialHost(reply.Port, 10*time.Second)
	if err != nil {
		return "", nil, err
	}
	// The kernel's NBD client takes TCP and Unix sockets, not vsock: it gets
	// one end of a Unix pair, and the guest relays the other to the host.
	kernelSide, relaySide, err := unixPair()
	if err != nil {
		conn.Close()
		return "", nil, err
	}
	go func() { io.Copy(relaySide, conn); relaySide.Close() }()
	go func() { io.Copy(conn, relaySide); conn.Close() }()
	disconnect, err := nbd.ConnectKernelConn(kernelSide, device)
	if err != nil {
		conn.Close()
		relaySide.Close()
		return "", nil, fmt.Errorf("attach %s: %w", image, err)
	}
	detach := func() error {
		err := disconnect()
		conn.Close()
		relaySide.Close()
		return err
	}
	if err := session.WaitForBlockDevice("/sys/block", device, 15*time.Second); err != nil {
		detach()
		return "", nil, err
	}
	return device, detach, nil
}

// unixPair returns the two ends of a connected Unix stream socket pair.
func unixPair() (*os.File, *os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, nil, err
	}
	return os.NewFile(uintptr(fds[0]), "nbd-kernel"), os.NewFile(uintptr(fds[1]), "nbd-relay"), nil
}

func waitFor(path string, limit time.Duration) error {
	for deadline := time.Now().Add(limit); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not appear", path)
		}
	}
}
