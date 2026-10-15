package session

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// OpenPTY opens a new terminal: its master, and the slave a process takes.
func OpenPTY() (master, slave *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if err := ioctl(master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("unlock pty: %w", err)
	}
	var n uint32
	if err := ioctl(master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("pty number: %w", err)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}

// SetWindowSize sets the size of the terminal f is.
func SetWindowSize(f *os.File, rows, cols uint16) {
	if rows == 0 || cols == 0 {
		return
	}
	size := [4]uint16{rows, cols, 0, 0}
	ioctl(f.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size)))
}

func ioctl(fd, request, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg); errno != 0 {
		return errno
	}
	return nil
}
