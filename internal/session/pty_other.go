//go:build !linux

package session

import (
	"errors"
	"os"
)

// OpenPTY opens a terminal in a session, which runs on Linux.
func OpenPTY() (master, slave *os.File, err error) {
	return nil, nil, errors.New("a session's terminal needs Linux")
}

// SetWindowSize sets the size of a session's terminal, on Linux.
func SetWindowSize(*os.File, uint16, uint16) {}
