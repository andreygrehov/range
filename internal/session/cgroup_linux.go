package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A native session's limits are a cgroup of its own, under range/ in the
// cgroup v2 hierarchy. The workload starts in it, and so does every command
// that range exec runs there; the NBD server, in range itself, stays out of
// it, so a busy workload cannot starve its own disk.

const cgroupRoot = "/sys/fs/cgroup"

type cgroup struct {
	path string
	dir  *os.File // the cgroup's directory, for CLONE_INTO_CGROUP
}

// newCgroup makes the cgroup of session id with limits, or nil when there
// are none.
func newCgroup(id string, limits Limits) (*cgroup, error) {
	if limits.CPUs <= 0 && limits.Memory <= 0 {
		return nil, nil
	}
	if _, err := os.Stat(filepath.Join(cgroupRoot, "cgroup.controllers")); err != nil {
		return nil, errors.New("--cpus and --memory need cgroup v2 here, or Range's VM: --runtime kvm")
	}
	parent := filepath.Join(cgroupRoot, "range")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("make a cgroup for the session: %w", err)
	}
	// A cgroup hands a controller to its children only if its own parent
	// does: the root to range/, range/ to each session.
	for _, dir := range []string{cgroupRoot, parent} {
		if err := enableControllers(dir); err != nil {
			return nil, err
		}
	}
	path := filepath.Join(parent, id)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, fmt.Errorf("make a cgroup for the session: %w", err)
	}
	cg := &cgroup{path: path}
	if limits.CPUs > 0 {
		const period = 100000
		quota := int(limits.CPUs * period)
		if err := cg.write("cpu.max", fmt.Sprintf("%d %d", quota, period)); err != nil {
			cg.remove()
			return nil, err
		}
	}
	if limits.Memory > 0 {
		if err := cg.write("memory.max", strconv.FormatUint(limits.Memory, 10)); err != nil {
			cg.remove()
			return nil, err
		}
		// Swap would let the workload go past its memory, slowly.
		cg.write("memory.swap.max", "0")
	}
	dir, err := os.Open(path)
	if err != nil {
		cg.remove()
		return nil, err
	}
	cg.dir = dir
	return cg, nil
}

// enableControllers hands cpu, memory and pids to the children of dir.
func enableControllers(dir string) error {
	have, err := os.ReadFile(filepath.Join(dir, "cgroup.controllers"))
	if err != nil {
		return err
	}
	var want []string
	for _, c := range []string{"cpu", "memory", "pids"} {
		if !strings.Contains(" "+strings.TrimSpace(string(have))+" ", " "+c+" ") {
			return fmt.Errorf("--cpus and --memory need the %s controller of cgroup v2, which %s lacks", c, dir)
		}
		want = append(want, "+"+c)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.subtree_control"), []byte(strings.Join(want, " ")), 0o644); err != nil {
		return fmt.Errorf("enable cgroup controllers in %s: %w", dir, err)
	}
	return nil
}

func (cg *cgroup) write(file, value string) error {
	if err := os.WriteFile(filepath.Join(cg.path, file), []byte(value), 0o644); err != nil {
		return fmt.Errorf("set %s of the session's cgroup: %w", file, err)
	}
	return nil
}

// fd is the cgroup's directory for SysProcAttr.CgroupFD, or -1.
func (cg *cgroup) fd() int {
	if cg == nil || cg.dir == nil {
		return -1
	}
	return int(cg.dir.Fd())
}

// remove ends whatever still runs in the cgroup and removes it.
func (cg *cgroup) remove() error {
	if cg == nil {
		return nil
	}
	if cg.dir != nil {
		cg.dir.Close()
	}
	os.WriteFile(filepath.Join(cg.path, "cgroup.kill"), []byte("1"), 0o644)
	var err error
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err = os.Remove(cg.path); err == nil || os.IsNotExist(err) {
			return nil
		}
	}
	return fmt.Errorf("remove the session's cgroup: %w", err)
}
