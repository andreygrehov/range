package vm

import (
	"syscall"
	"unsafe"
)

// TerminalSize is the size of the terminal on fd.
func TerminalSize(fd int) (rows, cols uint16, ok bool) {
	var size [4]uint16
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&size)))
	return size[0], size[1], errno == 0
}

// rawTerminal puts the terminal in raw mode, as ssh does for a remote pty,
// and returns how to put it back.
func rawTerminal(fd int) func() {
	var saved syscall.Termios
	if ioctlTermios(fd, getTermios, &saved) != nil {
		return func() {}
	}
	raw := saved
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	ioctlTermios(fd, setTermios, &raw)
	return func() { ioctlTermios(fd, setTermios, &saved) }
}

func ioctlTermios(fd int, request uintptr, t *syscall.Termios) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), request, uintptr(unsafe.Pointer(t))); errno != 0 {
		return errno
	}
	return nil
}
