//go:build !(darwin && cgo)

package main

import (
	"errors"

	"github.com/andreygrehov/range/internal/session"
)

func commandVMHost(_ []string) error {
	return errors.New(session.VMHostCommand + " needs a macOS build of range with cgo")
}
