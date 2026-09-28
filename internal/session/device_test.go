package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAllocateNBDDevice(t *testing.T) {
	makeTree := func(t *testing.T, devices map[string]bool) (string, string) {
		t.Helper()
		sysBlock, devDir := t.TempDir(), t.TempDir()
		for name, busy := range devices {
			if err := os.MkdirAll(filepath.Join(sysBlock, name), 0o755); err != nil {
				t.Fatal(err)
			}
			if busy {
				if err := os.WriteFile(filepath.Join(sysBlock, name, "pid"), []byte("123"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(devDir, name), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return sysBlock, devDir
	}

	t.Run("picks the lowest free device", func(t *testing.T) {
		sysBlock, devDir := makeTree(t, map[string]bool{"nbd0": true, "nbd1": true, "nbd2": false, "nbd10": false})
		device, err := AllocateNBDDevice(sysBlock, devDir)
		if err != nil {
			t.Fatalf("allocate: %v", err)
		}
		if filepath.Base(device) != "nbd2" {
			t.Fatalf("device = %s, want nbd2 (numeric order, not lexical)", device)
		}
	})

	t.Run("all busy", func(t *testing.T) {
		sysBlock, devDir := makeTree(t, map[string]bool{"nbd0": true, "nbd1": true})
		if _, err := AllocateNBDDevice(sysBlock, devDir); err == nil {
			t.Fatal("expected an error when every device is in use")
		}
	})

	t.Run("module not loaded", func(t *testing.T) {
		sysBlock, devDir := t.TempDir(), t.TempDir()
		_, err := AllocateNBDDevice(sysBlock, devDir)
		if err == nil || !strings.Contains(err.Error(), "modprobe") {
			t.Fatalf("error = %v, want advice about loading the nbd module", err)
		}
	})
}

func TestWaitForBlockDevice(t *testing.T) {
	sysBlock := t.TempDir()
	if err := os.MkdirAll(filepath.Join(sysBlock, "nbd0"), 0o755); err != nil {
		t.Fatal(err)
	}
	sizePath := filepath.Join(sysBlock, "nbd0", "size")
	if err := os.WriteFile(sizePath, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WaitForBlockDevice(sysBlock, "/dev/nbd0", 150*time.Millisecond); err == nil {
		t.Fatal("a zero-sized device must not count as ready")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		os.WriteFile(sizePath, []byte("2097152\n"), 0o644)
	}()
	if err := WaitForBlockDevice(sysBlock, "/dev/nbd0", 3*time.Second); err != nil {
		t.Fatalf("device never became ready: %v", err)
	}
}

func TestNBDIndexOrdersNumerically(t *testing.T) {
	if nbdIndex("nbd2") >= nbdIndex("nbd10") {
		t.Fatal("nbd2 must sort before nbd10")
	}
	if nbdIndex("nbdX") != 1<<30 {
		t.Fatal("an unparseable device name must sort last")
	}
}
