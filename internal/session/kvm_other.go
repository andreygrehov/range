//go:build !linux

package session

import "errors"

// newKVM reports that only Linux runs sessions in QEMU with KVM.
func newKVM() (Runtime, bool) { return nil, false }

func kvmUnavailable() error { return errors.New("Range's VM with KVM needs Linux") }

func kvmMissing() string { return "not Linux" }
