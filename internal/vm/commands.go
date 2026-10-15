package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/andreygrehov/range/internal/nbd"
)

// A session in the VM runs a command it lacks from the image that holds it,
// as a native one does, but the guest has no catalog and no credentials, and
// a running VM takes no new disk. The guest therefore asks the host on the
// commands port: for the names of the commands, for the image of one, and to
// open an image. The host opens it and serves it over NBD on a port of its
// own, from portImages on, and the guest's kernel attaches that stream as a
// block device of its own.

// Commands is what the host does for the guest's commands from other images.
type Commands interface {
	Names(ctx context.Context) ([]string, error)
	Find(ctx context.Context, command, version string) (image, program string, err error)
	Open(ctx context.Context, image string) (ImageSource, error)
}

// ImageSource is an opened image, for NBD.
type ImageSource interface {
	io.ReaderAt
	Size() int64
	Close() error
}

// CommandRequest is one question on the commands port: Op is names, find
// or open.
type CommandRequest struct {
	Op      string `json:"op"`
	Command string `json:"command,omitempty"`
	Version string `json:"version,omitempty"`
	Image   string `json:"image,omitempty"`
}

// CommandReply answers it.
type CommandReply struct {
	Names   []string `json:"names,omitempty"`
	Image   string   `json:"image,omitempty"`
	Program string   `json:"program,omitempty"`
	Port    uint32   `json:"port,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// maxImages bounds the images one session opens: one port each.
const maxImages = portSpan - portImages

// serveCommands answers the guest on its commands port until stop.
func serveCommands(ctx context.Context, m Machine, base uint32, c Commands) (stop func(), err error) {
	l, err := m.Listen(base + portCommands)
	if err != nil {
		return nil, err
	}
	s := &commandServer{m: m, base: base, c: c}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.answer(ctx, conn)
		}
	}()
	return func() {
		l.Close()
		s.mu.Lock()
		for _, close := range s.closers {
			close()
		}
		s.mu.Unlock()
	}, nil
}

type commandServer struct {
	m       Machine
	base    uint32
	c       Commands
	mu      sync.Mutex
	opened  int
	closers []func()
}

func (s *commandServer) answer(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	var req CommandRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}
	var reply CommandReply
	var err error
	switch req.Op {
	case "names":
		reply.Names, err = s.c.Names(ctx)
	case "find":
		reply.Image, reply.Program, err = s.c.Find(ctx, req.Command, req.Version)
	case "open":
		reply.Port, err = s.open(ctx, req.Image)
	default:
		err = fmt.Errorf("no such request %q", req.Op)
	}
	if err != nil {
		reply.Error = err.Error()
	}
	json.NewEncoder(conn).Encode(reply)
}

// open opens an image and serves it, once, on a port of its own.
func (s *commandServer) open(ctx context.Context, image string) (uint32, error) {
	s.mu.Lock()
	if s.opened == maxImages {
		s.mu.Unlock()
		return 0, fmt.Errorf("a session opens at most %d images", maxImages)
	}
	port := s.base + portImages + uint32(s.opened)
	s.opened++
	s.mu.Unlock()
	src, err := s.c.Open(ctx, image)
	if err != nil {
		return 0, err
	}
	l, err := s.m.Listen(port)
	if err != nil {
		src.Close()
		return 0, err
	}
	s.mu.Lock()
	s.closers = append(s.closers, func() { l.Close(); src.Close() })
	s.mu.Unlock()
	go func() {
		conn, err := l.Accept()
		l.Close()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := nbd.ServerHandshake(conn, src.Size()); err != nil {
			return
		}
		nbd.Serve(conn, src, src.Size())
	}()
	return port, nil
}
