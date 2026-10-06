package vm

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// hostCID is the vsock address of the host, seen from a guest. afVSOCK is
// AF_VSOCK, which package syscall defines on some architectures only.
const (
	hostCID = 2
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

func dialHost(port uint32) (*os.File, error) {
	fd, err := syscall.Socket(afVSOCK, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	addr := sockaddrVM{family: afVSOCK, port: port, cid: hostCID}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd),
		uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	if errno != 0 {
		syscall.Close(fd)
		return nil, fmt.Errorf("vsock connect to port %d: %w", port, errno)
	}
	return os.NewFile(uintptr(fd), fmt.Sprintf("vsock:%d", port)), nil
}

// CloseWrite ends the sending half of a vsock connection, so the host reads
// EOF while the guest can still read.
func CloseWrite(f *os.File) error {
	return syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
}
