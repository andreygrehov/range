package session

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
)

// Limits bound what a session takes of this machine, as docker run's flags of
// the same names do. In Range's VM they size the VM; natively a cgroup holds
// the workload. Zero is no limit.
type Limits struct {
	CPUs   float64 // --cpus: CPUs, in fractions of one
	Memory uint64  // --memory: bytes
	Disk   uint64  // --storage-opt size=: bytes the writable layer holds
}

// Set says whether any limit is set.
func (l Limits) Set() bool { return l.CPUs > 0 || l.Memory > 0 || l.Disk > 0 }

// minMemory is the least memory a session runs in: Range's VM boots in it.
const minMemory = 256 << 20

// ParseCPUs reads --cpus: a number of CPUs, such as 2 or 0.5.
func ParseCPUs(v string) (float64, error) {
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n <= 0 || n > 1024 {
		return 0, fmt.Errorf("--cpus %q: want a number of CPUs, such as 2 or 0.5", v)
	}
	return n, nil
}

// ParseMemory reads --memory: a size, such as 2g or 512MiB.
func ParseMemory(v string) (uint64, error) {
	n, err := bytesize.Parse(v)
	if err != nil {
		return 0, fmt.Errorf("--memory %q: want a size, such as 2g or 512MiB", v)
	}
	if n < minMemory {
		return 0, fmt.Errorf("--memory %s: a session needs at least %s", v, bytesize.Format(minMemory))
	}
	return uint64(n), nil
}

// ParseStorageOpt reads --storage-opt, as docker run takes it. Range knows
// one option, size=SIZE, the most the writable layer holds.
func ParseStorageOpt(v string) (uint64, error) {
	key, value, _ := strings.Cut(v, "=")
	if key != "size" {
		return 0, fmt.Errorf("--storage-opt %q: Range takes size=SIZE, such as size=10g", v)
	}
	n, err := bytesize.Parse(value)
	if err != nil || n < 1<<20 {
		return 0, fmt.Errorf("--storage-opt %q: want a size of 1MiB or more, such as size=10g", v)
	}
	return uint64(n), nil
}

// ParseTimeout reads --timeout: a duration, such as 30m or 1h30m, or seconds.
func ParseTimeout(v string) (time.Duration, error) {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--timeout %q: want a duration, such as 30m, or seconds", v)
	}
	return d, nil
}

// NetworkNone is --network none: a session with no network of its own.
const NetworkNone = "none"

// ParseNetwork reads --network, as docker run takes it: none, or default
// for the network the session has without the flag.
func ParseNetwork(v string) (string, error) {
	switch v {
	case "none":
		return NetworkNone, nil
	case "default", "bridge", "host":
		return "", nil
	}
	return "", fmt.Errorf("--network %q: none, or default", v)
}

// TimeoutStatus is the exit status of a session its --timeout ended, as
// timeout(1) gives.
const TimeoutStatus = ExitStatus(124)
