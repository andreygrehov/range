package session

import (
	"os"
	"runtime"
	"syscall"
)

// dieWithParent makes the process started with attr receive SIGKILL when
// the thread that starts it ends: when range dies, killed or not, unshare
// goes with it. The caller keeps that thread for as long as the process
// runs (runInNamespaces locks it).
func dieWithParent(attr *syscall.SysProcAttr) *syscall.SysProcAttr {
	if attr == nil {
		attr = &syscall.SysProcAttr{}
	}
	attr.Pdeathsig = syscall.SIGKILL
	return attr
}

// exitWithParent asks for SIGKILL when this process's parent ends. In the
// first process of a PID namespace, that ends the whole namespace: once
// unshare is gone, nothing of the session outlives it. The request belongs
// to a thread, and the workload is exec'd later: this goroutine stays on its
// thread from here on, so the exec happens on the thread that asked.
func exitWithParent() {
	runtime.LockOSThread()
	parent := os.Getppid()
	syscall.RawSyscall(syscall.SYS_PRCTL, syscall.PR_SET_PDEATHSIG, uintptr(syscall.SIGKILL), 0)
	// The parent may have ended before the request; then no signal comes.
	if os.Getppid() != parent {
		os.Exit(1)
	}
}
