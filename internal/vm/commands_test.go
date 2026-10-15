package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
)

// loopback is a machine whose vsock ports are TCP ports on 127.0.0.1.
type loopback struct {
	mu    sync.Mutex
	addrs map[uint32]string
}

func (l *loopback) Listen(port uint32) (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	l.addrs[port] = ln.Addr().String()
	l.mu.Unlock()
	return ln, nil
}

func (l *loopback) Connect(uint32) (net.Conn, error) { return nil, fmt.Errorf("unused") }
func (l *loopback) Stopped() <-chan struct{}         { return nil }

func (l *loopback) dial(t *testing.T, port uint32) net.Conn {
	l.mu.Lock()
	addr := l.addrs[port]
	l.mu.Unlock()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

type source struct{ *bytes.Reader }

func (s source) Size() int64  { return s.Reader.Size() }
func (s source) Close() error { return nil }

type fakeCommands struct{}

func (fakeCommands) Names(context.Context) ([]string, error) { return []string{"nvcc"}, nil }
func (fakeCommands) Find(_ context.Context, command, _ string) (string, string, error) {
	if command != "nvcc" {
		return "", "", fmt.Errorf("%s: not in the map", command)
	}
	return "nvidia/cuda:12.8", "nvcc", nil
}
func (fakeCommands) Open(context.Context, string) (ImageSource, error) {
	return source{bytes.NewReader(make([]byte, 4096))}, nil
}

func TestServeCommands(t *testing.T) {
	m := &loopback{addrs: map[uint32]string{}}
	stop, err := serveCommands(context.Background(), m, 64, fakeCommands{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	ask := func(req CommandRequest) CommandReply {
		conn := m.dial(t, 64+portCommands)
		defer conn.Close()
		json.NewEncoder(conn).Encode(req)
		var reply CommandReply
		if err := json.NewDecoder(conn).Decode(&reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	if r := ask(CommandRequest{Op: "names"}); fmt.Sprint(r.Names) != "[nvcc]" {
		t.Errorf("names: %+v", r)
	}
	if r := ask(CommandRequest{Op: "find", Command: "nvcc"}); r.Image != "nvidia/cuda:12.8" || r.Program != "nvcc" {
		t.Errorf("find: %+v", r)
	}
	if r := ask(CommandRequest{Op: "find", Command: "gti"}); r.Error == "" {
		t.Errorf("an unknown command: %+v", r)
	}
	r := ask(CommandRequest{Op: "open", Image: "nvidia/cuda:12.8"})
	if r.Port != 64+portImages {
		t.Fatalf("open: %+v", r)
	}
	// The image's port speaks NBD.
	conn := m.dial(t, r.Port)
	defer conn.Close()
	magic := make([]byte, 8)
	if _, err := io.ReadFull(conn, magic); err != nil || string(magic) != "NBDMAGIC" {
		t.Errorf("the image's port says %q, %v", magic, err)
	}
}
