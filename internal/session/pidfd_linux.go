package session

import "syscall"

// Linux has these two numbers on every architecture.
const (
	sysPidfdSendSignal = 424
	sysPidfdOpen       = 434
)

// processHandle refers to process pid for as long as the handle is open, so
// a signal sent through it never reaches another process that took the
// number later.
type processHandle int

func openProcess(pid int) (processHandle, error) {
	fd, _, errno := syscall.Syscall(sysPidfdOpen, uintptr(pid), 0, 0)
	if errno != 0 {
		return -1, errno
	}
	return processHandle(fd), nil
}

func (h processHandle) signal(sig syscall.Signal) error {
	if _, _, errno := syscall.Syscall6(sysPidfdSendSignal, uintptr(h), uintptr(sig), 0, 0, 0, 0); errno != 0 {
		return errno
	}
	return nil
}

func (h processHandle) close() { syscall.Close(int(h)) }
