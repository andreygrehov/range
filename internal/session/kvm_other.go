//go:build !linux

package session

// newKVM reports that only Linux runs sessions in QEMU with KVM.
func newKVM() (Runtime, bool) { return nil, false }
