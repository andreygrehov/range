//go:build !linux

package main

import (
	"errors"

	"github.com/andreygrehov/range/internal/session"
)

func commandVMGuest(_ []string) error {
	return errors.New(session.VMGuestCommand + " runs only inside the Linux VM Range boots")
}
