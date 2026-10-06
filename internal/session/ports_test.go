package session

import (
	"bufio"
	"io"
	"net"
	"strconv"
	"testing"
)

func TestParsePort(t *testing.T) {
	for spec, want := range map[string]Port{
		"8000":              {"127.0.0.1", 8000, 8000},
		"8080:80":           {"127.0.0.1", 8080, 80},
		"0.0.0.0:8080:80":   {"0.0.0.0", 8080, 80},
		"::1:8080:80":       {}, // an IPv6 address needs brackets docker does not take either
		"localhost:8080:80": {},
		"80:0":              {},
		"70000":             {},
		"a:b":               {},
		"1:2:3:4":           {},
	} {
		got, err := ParsePort(spec)
		if want == (Port{}) {
			if err == nil {
				t.Errorf("ParsePort(%q) = %+v, want an error", spec, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("ParsePort(%q) = %+v, %v, want %+v", spec, got, err, want)
		}
	}
}

func TestParseEnv(t *testing.T) {
	t.Setenv("RANGE_TEST_FROM_HERE", "here")
	for spec, want := range map[string][3]string{
		"KEY=VALUE":            {"KEY", "VALUE", "set"},
		"EMPTY=":               {"EMPTY", "", "set"},
		"A=b=c":                {"A", "b=c", "set"},
		"RANGE_TEST_FROM_HERE": {"RANGE_TEST_FROM_HERE", "here", "set"},
		"RANGE_TEST_UNSET":     {"RANGE_TEST_UNSET", "", "unset"},
	} {
		key, value, ok, err := ParseEnv(spec)
		if err != nil || key != want[0] || value != want[1] || ok != (want[2] == "set") {
			t.Errorf("ParseEnv(%q) = %q, %q, %v, %v", spec, key, value, ok, err)
		}
	}
	for _, bad := range []string{"", "=x", "A B=c"} {
		if _, _, _, err := ParseEnv(bad); err == nil {
			t.Errorf("ParseEnv(%q) took it", bad)
		}
	}
}

// A published port relays both ways, one connection after another.
func TestPublishRelays(t *testing.T) {
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
	hostPort := free.Addr().(*net.TCPAddr).Port
	free.Close()
	stop, err := Publish(Port{HostIP: "127.0.0.1", HostPort: hostPort, Port: 1},
		func() (io.ReadWriteCloser, error) { return net.Dial("tcp", inside.Addr().String()) })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort)))
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
	if _, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))); err == nil {
		t.Fatal("the port still answers after stop")
	}
}
