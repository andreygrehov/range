//go:build !linux

package session

import (
	"errors"
	"syscall"
)

type processHandle int

func openProcess(int) (processHandle, error) { return -1, errors.New("needs Linux") }

func (processHandle) signal(syscall.Signal) error { return errors.New("needs Linux") }

func (processHandle) close() {}
