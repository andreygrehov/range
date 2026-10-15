package session

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// loopbackUp brings up lo, which a new network namespace has down: the only
// network a session without one has, where its proxy is.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var ifr [40]byte // struct ifreq: the name, then the flags
	copy(ifr[:], "lo")
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return fmt.Errorf("read the flags of lo: %w", errno)
	}
	flags := (*uint16)(unsafe.Pointer(&ifr[syscall.IFNAMSIZ]))
	*flags |= syscall.IFF_UP | syscall.IFF_RUNNING
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return fmt.Errorf("bring up lo: %w", errno)
	}
	return nil
}

// waitForOwnNetwork waits until process pid is in a network namespace other
// than this process's: unshare(1) makes its own a moment after it starts.
func waitForOwnNetwork(pid int) error {
	mine, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return err
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if theirs, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", pid)); err == nil && theirs != mine {
			return nil
		}
	}
	return fmt.Errorf("the session did not get a network namespace of its own")
}
