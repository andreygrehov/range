//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// runAsShim replaces this process with range itself, found through the
// shim's link, running the command the shim is named for.
func runAsShim(name string, args []string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if self, err = filepath.EvalSymlinks(self); err != nil {
		return err
	}
	return syscall.Exec(self, append([]string{"range", "x", name}, args...), os.Environ())
}

// refreshShims starts range shims on its own, detached from the shell that
// is starting, which goes on without waiting for it.
func refreshShims() {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, "shims")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		cmd.Process.Release()
	}
}
