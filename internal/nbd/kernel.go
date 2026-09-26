package nbd

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	ioctlSetSock       = 0xab00
	ioctlSetBlockSize  = 0xab01
	ioctlDoIt          = 0xab03
	ioctlClearSock     = 0xab04
	ioctlSetSizeBlocks = 0xab07
	ioctlDisconnect    = 0xab08
	ioctlSetTimeout    = 0xab09
	ioctlSetFlags      = 0xab0a

	deviceBlockSize = 4096
)

func ioctl(fd uintptr, request, argument uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, argument); errno != 0 {
		return errno
	}
	return nil
}

// doItShielded runs NBD_DO_IT, which blocks for the life of the device, on
// an OS thread of its own with every signal blocked. The kernel waits in DO_IT
// interruptibly, and if a signal lands on that thread it shuts the socket down
// before returning: the device dies and every read fails with EIO. Range starts
// child processes (mount, unshare) right after attaching, and their SIGCHLD
// can be delivered to any thread that has it unblocked, including this one.
// nbd-client avoids the same thing by blocking signals in the process that
// calls DO_IT. The thread is never unlocked, so it exits with the goroutine
// instead of going back to the scheduler with a mask Go does not expect.
func doItShielded(dev *os.File) error {
	runtime.LockOSThread()
	var number uintptr
	switch runtime.GOARCH {
	case "amd64":
		number = 14 // rt_sigprocmask
	case "arm64", "riscv64":
		number = 135
	default:
		return fmt.Errorf("nbd: no rt_sigprocmask number for %s", runtime.GOARCH)
	}
	all := ^uint64(0)
	const sigBlock = 0
	if _, _, errno := syscall.RawSyscall6(number, sigBlock, uintptr(unsafe.Pointer(&all)), 0, 8, 0, 0); errno != 0 {
		return fmt.Errorf("nbd: block signals: %w", errno)
	}
	return ioctl(dev.Fd(), ioctlDoIt, 0)
}

// kernelTimeout is how long the kernel waits for any one request before it
// gives up on the connection and fails every outstanding read with EIO. It is a
// backstop: a server that dies closes the socket and fails reads at once, but a
// connection that stays open while nothing answers — a stalled tunnel, a wedged
// server — would otherwise leave every process reading the filesystem in
// uninterruptible sleep, forever. It is set well above a demand read's worst
// case (three attempts of --request-timeout, 30s by default) plus time queued
// behind other reads, because once it fires the mount is gone.
const kernelTimeout = 3 * time.Minute

// kernelSetSock configures the kernel's nbd device and hands it a socket that
// is already in transmission phase. NBD_DO_IT is left to the caller, because
// it blocks until the device is disconnected. The device is always read-only,
// whatever the server advertised: Range never writes to an artifact, and the
// kernel should refuse to try.
func kernelSetSock(dev *os.File, sock int, size int64, flags uint16) error {
	flags |= flagHasFlags | flagReadOnly
	if err := ioctl(dev.Fd(), ioctlSetBlockSize, deviceBlockSize); err != nil {
		return fmt.Errorf("NBD_SET_BLKSIZE: %w", err)
	}
	if err := ioctl(dev.Fd(), ioctlSetSizeBlocks, uintptr(size/deviceBlockSize)); err != nil {
		return fmt.Errorf("NBD_SET_SIZE_BLOCKS: %w", err)
	}
	if err := ioctl(dev.Fd(), ioctlSetFlags, uintptr(flags)); err != nil {
		return fmt.Errorf("NBD_SET_FLAGS: %w", err)
	}
	if err := ioctl(dev.Fd(), ioctlSetTimeout, uintptr(kernelTimeout/time.Second)); err != nil {
		return fmt.Errorf("NBD_SET_TIMEOUT: %w", err)
	}
	if err := ioctl(dev.Fd(), ioctlSetSock, uintptr(sock)); err != nil {
		return fmt.Errorf("NBD_SET_SOCK: %w", err)
	}
	return nil
}

// ConnectKernel dials an NBD server over TCP, negotiates, and attaches the
// socket to a kernel device. It returns once the device is live; NBD_DO_IT
// then runs in the background until disconnect is called or the connection
// ends. There is no reconnect logic, on purpose: on macOS the connection rides
// the same ssh session as the guest process, so it cannot drop while the
// session it serves is still alive.
func ConnectKernel(address, device string) (disconnect func() error, err error) {
	conn, err := net.DialTimeout("tcp", address, 15*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect to the NBD export at %s: %w", address, err)
	}
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	size, flags, err := clientHandshake(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.SetDeadline(time.Time{})
	if size <= 0 || size%deviceBlockSize != 0 {
		conn.Close()
		return nil, fmt.Errorf("nbd: export size %d is not a multiple of %d", size, deviceBlockSize)
	}
	socketFile, err := conn.(*net.TCPConn).File()
	conn.Close() // File dup'd the descriptor; the kernel gets the duplicate
	if err != nil {
		return nil, err
	}
	sock := int(socketFile.Fd())
	if err := syscall.SetNonblock(sock, false); err != nil {
		socketFile.Close()
		return nil, err
	}
	dev, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		socketFile.Close()
		return nil, fmt.Errorf("open %s (is the nbd module loaded?): %w", device, err)
	}
	if err := kernelSetSock(dev, sock, size, flags); err != nil {
		dev.Close()
		socketFile.Close()
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- doItShielded(dev) }()
	var once sync.Once
	disconnect = func() error {
		once.Do(func() {
			if other, err := os.OpenFile(device, os.O_RDWR, 0); err == nil {
				ioctl(other.Fd(), ioctlDisconnect, 0)
				other.Close()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
			ioctl(dev.Fd(), ioctlClearSock, 0)
			dev.Close()
			socketFile.Close()
		})
		return nil
	}
	return disconnect, nil
}

// Attach wires the kernel's nbd device to an in-process server over a
// socketpair. The kernel starts directly in transmission phase.
func Attach(ctx context.Context, device string, source io.ReaderAt, size int64) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("range nbd attach requires Linux (running on %s); use \"range nbd serve\" instead", runtime.GOOS)
	}
	if size%deviceBlockSize != 0 {
		return fmt.Errorf("artifact size %d is not a multiple of %d; the kernel needs whole blocks", size, deviceBlockSize)
	}
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return fmt.Errorf("socketpair: %w", err)
	}
	kernelSide, serverSide := pair[0], pair[1]
	defer syscall.Close(kernelSide)

	dev, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		syscall.Close(serverSide)
		return fmt.Errorf("open %s (is the nbd module loaded?): %w", device, err)
	}
	defer dev.Close()

	if err := kernelSetSock(dev, kernelSide, size, flagHasFlags|flagReadOnly); err != nil {
		syscall.Close(serverSide)
		return err
	}

	conn := os.NewFile(uintptr(serverSide), "nbd-socket")
	served := make(chan error, 1)
	go func() {
		defer conn.Close()
		served <- Serve(conn, source, size)
	}()

	// NBD_DO_IT blocks until the device is disconnected.
	done := make(chan error, 1)
	go func() { done <- doItShielded(dev) }()

	select {
	case <-ctx.Done():
		disconnector, err := os.OpenFile(device, os.O_RDWR, 0)
		if err == nil {
			ioctl(disconnector.Fd(), ioctlDisconnect, 0)
			disconnector.Close()
		}
		<-done
	case err := <-done:
		if err != nil {
			ioctl(dev.Fd(), ioctlClearSock, 0)
			return fmt.Errorf("NBD_DO_IT: %w", err)
		}
	}
	ioctl(dev.Fd(), ioctlClearSock, 0)
	select {
	case err := <-served:
		return err
	case <-time.After(2 * time.Second):
		return nil
	}
}
