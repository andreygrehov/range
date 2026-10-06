//go:build !linux

package session

import "syscall"

// Sessions run on Linux only; elsewhere these do nothing.
func dieWithParent(attr *syscall.SysProcAttr) *syscall.SysProcAttr { return attr }
func exitWithParent()                                              {}
