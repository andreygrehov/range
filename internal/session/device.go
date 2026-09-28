package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AllocateNBDDevice returns the first nbd device the kernel is not already
// using. An in-use device publishes a pid file in sysfs.
func AllocateNBDDevice(sysBlock, devDir string) (string, error) {
	entries, err := os.ReadDir(sysBlock)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", sysBlock, err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "nbd") {
			names = append(names, entry.Name())
		}
	}
	if len(names) == 0 {
		return "", errors.New("no nbd devices found; load the module with: sudo modprobe nbd nbds_max=16")
	}
	sort.Slice(names, func(i, j int) bool { return nbdIndex(names[i]) < nbdIndex(names[j]) })
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(sysBlock, name, "pid")); err == nil {
			continue // the kernel is already serving this one
		}
		device := filepath.Join(devDir, name)
		if _, err := os.Stat(device); err != nil {
			continue
		}
		return device, nil
	}
	return "", errors.New("every nbd device is busy; raise nbds_max or disconnect one")
}

func nbdIndex(name string) int {
	index, err := strconv.Atoi(strings.TrimPrefix(name, "nbd"))
	if err != nil {
		return 1 << 30
	}
	return index
}

// WaitForBlockDevice waits for the kernel to publish a non-zero size, which is
// how we know NBD_DO_IT has taken the socket and the device is readable.
func WaitForBlockDevice(sysBlock, device string, timeout time.Duration) error {
	sizePath := filepath.Join(sysBlock, filepath.Base(device), "size")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(sizePath); err == nil {
			if sectors, _ := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); sectors > 0 {
				return nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("%s did not become ready in %s", device, timeout)
}

// waitForDeviceRelease blocks until the kernel stops publishing a pid for the
// device, which is how it marks an nbd device as back in the free pool.
func waitForDeviceRelease(sysBlock, device string, timeout time.Duration) error {
	pidPath := filepath.Join(sysBlock, filepath.Base(device), "pid")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("%s is still in use after %s", device, timeout)
}
