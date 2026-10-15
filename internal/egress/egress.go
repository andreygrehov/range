// Package egress is the way out of a session that has no network of its
// own: an HTTP proxy, on this machine, that lets through only the hosts a
// session allows. The session reaches it from inside, through its standard
// proxy variables: curl, git, pip, npm, go and cargo all honor them. Nothing
// else leaves: the session has no route, no resolver and no other address.
package egress

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Port is where the proxy is, on 127.0.0.1 inside a session.
const Port = 3128

// Env are the variables that send a session's traffic to the proxy.
func Env() []string {
	proxy := "http://127.0.0.1:" + strconv.Itoa(Port)
	noProxy := "localhost,127.0.0.1,::1"
	return []string{
		"HTTP_PROXY=" + proxy, "HTTPS_PROXY=" + proxy, "http_proxy=" + proxy, "https_proxy=" + proxy,
		"NO_PROXY=" + noProxy, "no_proxy=" + noProxy,
	}
}

// Policy is the hosts a session may reach.
type Policy struct {
	rules []rule
}

type rule struct {
	host     string // lower case; "*.example.com" takes its subdomains
	wildcard bool
	port     int // 0 takes 80 and 443
}

// Parse reads --allow-host values: HOST or HOST:PORT, where HOST may start
// with "*." for every subdomain of a domain.
func Parse(hosts []string) (Policy, error) {
	var p Policy
	for _, h := range hosts {
		r, err := parseRule(h)
		if err != nil {
			return Policy{}, err
		}
		p.rules = append(p.rules, r)
	}
	return p, nil
}

func parseRule(s string) (rule, error) {
	bad := func() (rule, error) {
		return rule{}, fmt.Errorf("--allow-host %q: want HOST or HOST:PORT, such as pypi.org, *.github.com or 10.0.0.5:8080", s)
	}
	host, port := strings.ToLower(strings.TrimSpace(s)), 0
	if h, p, err := net.SplitHostPort(host); err == nil {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return bad()
		}
		host, port = h, n
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	r := rule{host: host, port: port}
	if rest, ok := strings.CutPrefix(host, "*."); ok {
		r.host, r.wildcard = rest, true
	}
	if r.host == "" || strings.ContainsAny(r.host, "*/@ ") || strings.Count(r.host, ".") == 0 && net.ParseIP(r.host) == nil && r.host != "localhost" {
		return bad()
	}
	return r, nil
}

// Allows says whether host:port may be reached.
func (p Policy) Allows(host string, port int) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.Trim(host, "[]")), ".")
	for _, r := range p.rules {
		if r.port != 0 && r.port != port || r.port == 0 && port != 80 && port != 443 {
			continue
		}
		if !r.wildcard && host == r.host || r.wildcard && strings.HasSuffix(host, "."+r.host) {
			return true
		}
	}
	return false
}

// Hosts are the rules, as given, for a session's banner.
func (p Policy) Hosts() []string {
	var out []string
	for _, r := range p.rules {
		h := r.host
		if r.wildcard {
			h = "*." + h
		}
		if r.port != 0 {
			h = net.JoinHostPort(h, strconv.Itoa(r.port))
		}
		out = append(out, h)
	}
	return out
}

// Proxy serves one session's requests.
type Proxy struct {
	Policy Policy
	// Log is told of each request: the host, its port and whether it went
	// through. It may be nil.
	Log func(host string, port int, allowed bool)
	// Dial reaches a host; nil is net.Dial, through this machine's own
	// proxy when HTTPS_PROXY or HTTP_PROXY names one.
	Dial func(network, address string) (net.Conn, error)
}

// Serve answers every connection l accepts until it is closed.
func (p *Proxy) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go p.ServeConn(conn)
	}
}

// ServeConn answers the requests on one connection.
func (p *Proxy) ServeConn(conn net.Conn) {
	defer conn.Close()
	in := bufio.NewReader(conn)
	for {
		req, err := http.ReadRequest(in)
		if err != nil {
			return
		}
		if req.Method == http.MethodConnect {
			p.tunnel(conn, in, req)
			return
		}
		if !p.forward(conn, req) {
			return
		}
	}
}

// tunnel answers CONNECT, which carries HTTPS: the proxy sees the host and
// port, and the session's TLS goes through it untouched.
func (p *Proxy) tunnel(conn net.Conn, in *bufio.Reader, req *http.Request) {
	host, port, ok := hostPort(req.Host, 443)
	if !p.check(conn, host, port, ok) {
		return
	}
	upstream, err := p.dial("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		reply(conn, http.StatusBadGateway, fmt.Sprintf("range: cannot reach %s:%d: %v\n", host, port, err))
		return
	}
	defer upstream.Close()
	io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")
	var done sync.WaitGroup
	done.Add(1)
	go func() {
		defer done.Done()
		io.Copy(upstream, in) // what the reader already holds, then the rest
		closeWrite(upstream)
	}()
	io.Copy(conn, upstream)
	closeWrite(conn)
	done.Wait()
}

// forward carries one plain HTTP request, and says whether the connection
// can take another.
func (p *Proxy) forward(conn net.Conn, req *http.Request) bool {
	if req.URL.Host == "" {
		reply(conn, http.StatusBadRequest, "range: this is a proxy: ask for an absolute URL\n")
		return false
	}
	host, port, ok := hostPort(req.URL.Host, 80)
	if req.URL.Scheme != "http" {
		ok = false
	}
	if !p.check(conn, host, port, ok) {
		return false
	}
	req.RequestURI = ""
	for _, h := range []string{"Proxy-Connection", "Proxy-Authorization", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade"} {
		req.Header.Del(h)
	}
	transport := &http.Transport{
		DialContext: func(_ context.Context, network, address string) (net.Conn, error) {
			return p.dial(network, address)
		},
		DisableCompression:    true,
		ResponseHeaderTimeout: 2 * time.Minute,
	}
	defer transport.CloseIdleConnections()
	resp, err := transport.RoundTrip(req)
	if err != nil {
		reply(conn, http.StatusBadGateway, fmt.Sprintf("range: cannot reach %s: %v\n", req.URL.Host, err))
		return false
	}
	defer resp.Body.Close()
	if err := resp.Write(conn); err != nil {
		return false
	}
	return !req.Close && !resp.Close
}

// check lets a request through, or refuses it and says why.
func (p *Proxy) check(conn net.Conn, host string, port int, ok bool) bool {
	allowed := ok && p.Policy.Allows(host, port)
	if p.Log != nil {
		p.Log(host, port, allowed)
	}
	if !allowed {
		reply(conn, http.StatusForbidden, fmt.Sprintf("range: %s:%d is not among the hosts this session may reach (--allow-host)\n", host, port))
	}
	return allowed
}

func (p *Proxy) dial(network, address string) (net.Conn, error) {
	if p.Dial != nil {
		return p.Dial(network, address)
	}
	return dialOut(address)
}

// dialOut connects to address, through this machine's proxy if it has one:
// a company network that only lets its proxy out lets the session out too.
func dialOut(address string) (net.Conn, error) {
	via, err := http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: address}})
	if err != nil || via == nil {
		return net.DialTimeout("tcp", address, 30*time.Second)
	}
	conn, err := net.DialTimeout("tcp", via.Host, 30*time.Second)
	if err != nil {
		return nil, err
	}
	connect := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: address}, Host: address, Header: http.Header{}}
	if u := via.User; u != nil {
		connect.SetBasicAuth(u.Username(), func() string { s, _ := u.Password(); return s }())
		connect.Header.Set("Proxy-Authorization", connect.Header.Get("Authorization"))
		connect.Header.Del("Authorization")
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if err := connect.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), connect)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		conn.Close()
		return nil, fmt.Errorf("this machine's proxy %s answered %s", via.Host, resp.Status)
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// hostPort splits host[:port], with port def when it names none.
func hostPort(s string, def int) (string, int, bool) {
	host, p, err := net.SplitHostPort(s)
	if err != nil {
		var addrErr *net.AddrError
		if errors.As(err, &addrErr) && strings.Contains(addrErr.Err, "missing port") {
			return strings.Trim(s, "[]"), def, s != ""
		}
		return "", 0, false
	}
	port, err := strconv.Atoi(p)
	return host, port, err == nil && port > 0 && port < 65536 && host != ""
}

func reply(conn net.Conn, status int, body string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	c.Close()
}
