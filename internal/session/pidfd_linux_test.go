package session

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestProcessHandle(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skip(err)
	}
	pid := cmd.Process.Pid
	if startTime(pid) == "" || startTime(pid) != startTime(pid) {
		t.Fatalf("start time of %d: %q", pid, startTime(pid))
	}
	h, err := openProcess(pid)
	if err != nil {
		t.Skipf("no pidfd on this kernel: %v", err)
	}
	defer h.close()
	if err := h.signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	// The process is gone: the handle reaches nothing else.
	if err := h.signal(syscall.SIGKILL); err == nil {
		t.Error("a signal reached a process that ended")
	}
	if startTime(os.Getpid()) == "" {
		t.Error("no start time for this process")
	}
}
