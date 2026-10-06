package vm

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// hostCID is the vsock address of the host, seen from a guest. afVSOCK is
// AF_VSOCK, which package syscall defines on some architectures only.
const (
	hostCID = 2
	anyCID  = 0xFFFFFFFF // VMADDR_CID_ANY
	afVSOCK = 40
)

// sockaddrVM is struct sockaddr_vm from <linux/vm_sockets.h>.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8
}

// DialHost connects to the host on a vsock port, retrying while the vsock
// transport comes up after boot. The connection is a blocking *os.File.
func DialHost(port uint32, timeout time.Duration) (*os.File, error) {
	deadline := time.Now().Add(timeout)
	for {
		f, err := dialHost(port)
		if err == nil || time.Now().After(deadline) {
			return f, err
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dialHost(port uint32) (*os.File, error) { return dial(hostCID, port) }

// dial connects to a vsock port of a VM, or of the host from a guest.
func dial(cid, port uint32) (*os.File, error) {
	fd, err := syscall.Socket(afVSOCK, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	addr := sockaddrVM{family: afVSOCK, port: port, cid: cid}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock connect to port %d: %w", port, errno)
	}
	return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port)), nil
}

// Listener accepts the host's connections to a vsock port.
type Listener struct{ fd int }

// ListenHost listens for the host on a vsock port.
func ListenHost(port uint32) (*Listener, error) {
	fd, err := syscall.Socket(afVSOCK, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	addr := sockaddrVM{family: afVSOCK, port: port, cid: anyCID}
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr)); errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock bind to port %d: %w", port, errno)
	}
	if err := syscall.Listen(fd, 16); err != nil {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock listen on port %d: %w", port, err)
	}
	return &Listener{fd: fd}, nil
}

// Accept waits for the next connection. syscall.Accept cannot be used: it
// refuses a peer address of a family it does not know, vsock among them.
func (l *Listener) Accept() (*os.File, error) {
	f, _, err := l.acceptFrom()
	return f, err
}

// acceptFrom waits for the next connection and says which VM made it.
func (l *Listener) acceptFrom() (*os.File, uint32, error) {
	var peer sockaddrVM
	size := uint32(unsafe.Sizeof(peer))
	nfd, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(l.fd),
		uintptr(unsafe.Pointer(&peer)), uintptr(unsafe.Pointer(&size)), syscall.SOCK_CLOEXEC, 0, 0)
	if errno != 0 {
		return nil, 0, errno
	}
	return os.NewFile(nfd, "vsock"), peer.cid, nil
}

// Close stops listening. Closing alone would not wake an Accept blocked on
// another thread; shutting the socket down does.
func (l *Listener) Close() error {
	syscall.Shutdown(l.fd, syscall.SHUT_RDWR)
	return syscall.Close(l.fd)
}

// CloseWrite ends the sending half of a vsock connection, so the host reads
// EOF while the guest can still read.
func CloseWrite(f *os.File) error {
	return syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
}

// vmListener is a host's vsock listener that takes connections from one VM
// only: on Linux, every VM on the host can reach the host's vsock ports.
type vmListener struct {
	l   *Listener
	cid uint32
}

func (v vmListener) Accept() (net.Conn, error) {
	for {
		f, peer, err := v.l.acceptFrom()
		if err != nil {
			return nil, err
		}
		if peer == v.cid {
			return vsockConn{f}, nil
		}
		f.Close()
	}
}

func (v vmListener) Close() error   { return v.l.Close() }
func (v vmListener) Addr() net.Addr { return vsockAddr{} }

// vsockConn is a vsock connection as a net.Conn. Deadlines are not
// supported: the session's own timeouts bound it.
type vsockConn struct{ *os.File }

func (vsockConn) LocalAddr() net.Addr                { return vsockAddr{} }
func (vsockConn) RemoteAddr() net.Addr               { return vsockAddr{} }
func (vsockConn) SetDeadline(t time.Time) error      { return nil }
func (vsockConn) SetReadDeadline(t time.Time) error  { return nil }
func (vsockConn) SetWriteDeadline(t time.Time) error { return nil }

type vsockAddr struct{}

func (vsockAddr) Network() string { return "vsock" }
func (vsockAddr) String() string  { return "vsock" }
