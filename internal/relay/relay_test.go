package relay

import (
	"bufio"
	"io"
	"net"
	"testing"
)

// A listener relays both ways, one connection after another.
func TestListenRelays(t *testing.T) {
	inside, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inside.Close()
	go func() {
		for {
			conn, err := inside.Accept()
			if err != nil {
				return
			}
			go func() {
				line, _ := bufio.NewReader(conn).ReadString('\n')
				io.WriteString(conn, "echo "+line)
				conn.Close()
			}()
		}
	}()
	free, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := free.Addr().String()
	free.Close()
	stop, err := Listen(addr, func() (io.ReadWriteCloser, error) { return net.Dial("tcp", inside.Addr().String()) })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(conn, "hi\n")
		reply, _ := io.ReadAll(conn)
		conn.Close()
		if string(reply) != "echo hi\n" {
			t.Fatalf("connection %d got %q", i, reply)
		}
	}
	stop()
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("the port still answers after stop")
	}
}
