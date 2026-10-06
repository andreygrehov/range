package session

import (
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/andreygrehov/range/internal/tool"
)

// The kernel loads the EROFS and overlay modules by itself when something
// mounts one of those filesystems. Nothing loads nbd that way: its devices
// do not exist until the module is in. So range, as root, loads it.

// nbdLoaded reports whether the nbd module is loaded or built in.
func nbdLoaded() bool {
	_, err := os.Stat("/sys/block/nbd0")
	return err == nil
}

// nbdLoadable reports whether modprobe can load nbd.
func nbdLoadable() bool {
	return exec.Command("modprobe", "-n", "-q", "nbd").Run() == nil
}

// LoadNBD loads the nbd module when it is missing, with devices for 16
// sessions at once, and waits until its devices exist.
func LoadNBD() error {
	if nbdLoaded() {
		return nil
	}
	if err := tool.Run("modprobe", "nbd", "nbds_max=16"); err != nil {
		return fmt.Errorf("load the nbd module: %w", err)
	}
	for deadline := time.Now().Add(5 * time.Second); !nbdLoaded(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the nbd module loaded, but /sys/block/nbd0 did not appear")
		}
	}
	return nil
}
