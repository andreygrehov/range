package tool

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Run executes a system utility and folds its output into any error.
func Run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), detail)
	}
	return nil
}

// KernelHasFilesystem reports whether this kernel can mount a filesystem type,
// either already registered or available as a module.
func KernelHasFilesystem(name string) bool {
	return KernelRegistered(name) || exec.Command("modprobe", "-n", "-q", name).Run() == nil
}

// KernelRegistered reports whether this kernel has a filesystem type now,
// built in or loaded.
func KernelRegistered(name string) bool {
	data, err := os.ReadFile("/proc/filesystems")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(strings.TrimPrefix(line, "nodev")) == name {
			return true
		}
	}
	return false
}
