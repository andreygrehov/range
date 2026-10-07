package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// moduleInitCompressedFile asks finit_module to decompress the module itself.
const moduleInitCompressedFile = 4

// LoadFilesystem loads the module of a filesystem the guest loads only when a
// session needs it, from the initramfs. A filesystem with no such module is
// left to the mount to report.
func LoadFilesystem(fsType string) error {
	f, err := os.Open(filepath.Join("/lib/modules", fsType+".ko.xz"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	params := []byte{0}
	_, _, errno := syscall.Syscall(sysFinitModule, f.Fd(),
		uintptr(unsafe.Pointer(&params[0])), moduleInitCompressedFile)
	if errno != 0 && errno != syscall.EEXIST {
		return fmt.Errorf("load %s: %w", f.Name(), errno)
	}
	return nil
}
