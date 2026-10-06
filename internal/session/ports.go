package session

import (
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/andreygrehov/range/internal/relay"
)

// Port publishes a port of the environment on this machine, as docker run -p
// does: connections to HostIP:HostPort reach Port inside.
type Port struct {
	HostIP   string
	HostPort int
	Port     int
}

// ParsePort reads PORT, HOSTPORT:PORT or IP:HOSTPORT:PORT. Without an IP the
// port is published on 127.0.0.1 only: a dev server is for this machine
// unless you say otherwise.
func ParsePort(spec string) (Port, error) {
	parts := strings.Split(spec, ":")
	p := Port{HostIP: "127.0.0.1"}
	var err error
	switch len(parts) {
	case 1:
		p.Port, err = parsePortNumber(parts[0])
		p.HostPort = p.Port
	case 2:
		if p.HostPort, err = parsePortNumber(parts[0]); err == nil {
			p.Port, err = parsePortNumber(parts[1])
		}
	case 3:
		p.HostIP = parts[0]
		if net.ParseIP(p.HostIP) == nil {
			return Port{}, fmt.Errorf("-p %q: %q is not an IP address", spec, p.HostIP)
		}
		if p.HostPort, err = parsePortNumber(parts[1]); err == nil {
			p.Port, err = parsePortNumber(parts[2])
		}
	default:
		return Port{}, fmt.Errorf("-p %q: want PORT, HOSTPORT:PORT or IP:HOSTPORT:PORT", spec)
	}
	if err != nil {
		return Port{}, fmt.Errorf("-p %q: %w", spec, err)
	}
	return p, nil
}

func parsePortNumber(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port", s)
	}
	return n, nil
}

// ParseEnv reads KEY=VALUE, or KEY alone for the value KEY has here. A KEY
// unset here is left out, as docker run -e leaves it out.
func ParseEnv(spec string) (key, value string, ok bool, err error) {
	key, value, hasValue := strings.Cut(spec, "=")
	if key == "" || strings.ContainsAny(key, " \t\n") {
		return "", "", false, fmt.Errorf("-e %q: want KEY=VALUE or KEY", spec)
	}
	if !hasValue {
		value, ok = os.LookupEnv(key)
		return key, value, ok, nil
	}
	return key, value, true, nil
}

// publishOnHost publishes ports where the environment already shares this
// machine's network, natively or in Lima: a port inside is the same port
// here, so only a port published under another number needs a relay.
func publishOnHost(sess *Session, ports []Port) error {
	for _, p := range ports {
		if p.HostPort == p.Port {
			continue
		}
		target := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Port))
		stop, err := Publish(p, func() (io.ReadWriteCloser, error) { return net.Dial("tcp", target) })
		if err != nil {
			return err
		}
		sess.Push(stop)
	}
	return nil
}

// Publish listens on p's host address and relays each connection to what
// dial returns, until stop is called.
func Publish(p Port, dial func() (io.ReadWriteCloser, error)) (stop func() error, err error) {
	stop, err = relay.Listen(net.JoinHostPort(p.HostIP, strconv.Itoa(p.HostPort)), dial)
	if err != nil {
		return nil, fmt.Errorf("publish port %d: %w", p.Port, err)
	}
	return stop, nil
}
