package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPolicy(t *testing.T) {
	p, err := Parse([]string{"pypi.org", "*.github.com", "10.0.0.5:8080", "Proxy.Golang.org."})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host  string
		port  int
		allow bool
	}{
		{"pypi.org", 443, true},
		{"pypi.org", 80, true},
		{"pypi.org", 22, false},         // a host alone is HTTP and HTTPS
		{"files.pypi.org", 443, false},  // no subdomains without *.
		{"api.github.com", 443, true},   // *. takes subdomains
		{"github.com", 443, false},      // and not the domain itself
		{"evilgithub.com", 443, false},  // nor a name that ends the same
		{"10.0.0.5", 8080, true},        // a port is that port
		{"10.0.0.5", 443, false},        //
		{"proxy.golang.org", 443, true}, // case and a final dot do not matter
		{"internal.corp", 443, false},   //
	} {
		if got := p.Allows(c.host, c.port); got != c.allow {
			t.Errorf("%s:%d: %v, want %v", c.host, c.port, got, c.allow)
		}
	}
	for _, bad := range []string{"", "*", "*.com.*", "http://pypi.org", "pypi.org:99999", "user@pypi.org", "intranet"} {
		if _, err := Parse([]string{bad}); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
}

// proxyFor runs a proxy that lets through only allowed, and sends every
// connection to target instead of where it asked.
func proxyFor(t *testing.T, target string, allowed ...string) string {
	t.Helper()
	policy, err := Parse(allowed)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	p := &Proxy{Policy: policy, Dial: func(network, _ string) (net.Conn, error) { return net.Dial(network, target) }}
	go p.Serve(l)
	return l.Addr().String()
}

func TestProxyForwardsHTTP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello from %s", r.Host)
	}))
	defer server.Close()
	addr := proxyFor(t, server.Listener.Addr().String(), "allowed.example")
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: addr})}}

	resp, err := client.Get("http://allowed.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello from allowed.example" {
		t.Errorf("allowed: %d %q", resp.StatusCode, body)
	}
	resp, err = client.Get("http://denied.example/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "--allow-host") {
		t.Errorf("denied: %d %q", resp.StatusCode, body)
	}
}

func TestProxyTunnels(t *testing.T) {
	// The other end of a tunnel: it echoes a line.
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	addr := proxyFor(t, echo.Addr().String(), "*.example.com")
	connect := func(host string) (string, string) {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", host, host)
		in := bufio.NewReader(c)
		resp, err := http.ReadResponse(in, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			return resp.Status, ""
		}
		fmt.Fprintln(c, "ping")
		line, _ := in.ReadString('\n')
		return resp.Status, line
	}
	if status, line := connect("api.example.com:443"); line != "ping\n" {
		t.Errorf("allowed tunnel: %s %q", status, line)
	}
	if status, _ := connect("example.org:443"); !strings.HasPrefix(status, "403") {
		t.Errorf("denied tunnel: %s", status)
	}
	if status, _ := connect("api.example.com:22"); !strings.HasPrefix(status, "403") {
		t.Errorf("a port not allowed: %s", status)
	}
}
