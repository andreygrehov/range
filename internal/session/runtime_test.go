package session

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeSelectionMatchesHost(t *testing.T) {
	host := Select()
	switch runtime.GOOS {
	case "linux":
		if host.Name() != "native" {
			t.Fatalf("runtime = %q, want native on Linux", host.Name())
		}
	case "darwin":
		// macOS is supported through a Linux VM: Range's own where the Mac
		// can boot one, Lima otherwise. The shell must not refuse outright;
		// missing pieces are provisioned rather than fatal.
		if _, own := newVZ(); own && host.Name() != "vz" || !own && host.Name() != "lima" {
			t.Fatalf("runtime = %q, own VM available: %v", host.Name(), own)
		}
		for _, req := range host.Requirements() {
			switch req.What {
			case "limactl", "guest binary", "codesign", "macOS 14+":
				continue
			}
			if !req.OK && !req.Fixable {
				t.Fatalf("unexpected fatal Requirement on macOS: %+v", req)
			}
		}
	default:
		if host.Name() == "native" {
			t.Fatalf("unsupported platform %s must not claim the native runtime", runtime.GOOS)
		}
	}
}

func TestUnsupportedHostExplainsWSL(t *testing.T) {
	// The message a Windows user gets has to name the supported route.
	hint := unsupportedHint()
	if runtime.GOOS == "windows" && !strings.Contains(hint, "WSL2") {
		t.Fatalf("hint = %q, want it to mention WSL2", hint)
	}
	if hint == "" {
		t.Fatal("unsupported hosts must get an explanation")
	}
}

// A workload's exit status, or the signal that ended it, becomes range's own.
func TestExitStatusOf(t *testing.T) {
	err := exitStatusOf(exec.Command("sh", "-c", "exit 7").Run())
	var status ExitStatus
	if !errors.As(err, &status) || status != 7 {
		t.Fatalf("exit 7 = %v", err)
	}
	err = exitStatusOf(exec.Command("sh", "-c", "kill -TERM $$").Run())
	if !errors.As(err, &status) || status != 128+15 {
		t.Fatalf("killed by SIGTERM = %v", err)
	}
	if err := exitStatusOf(exec.Command("true").Run()); err != nil {
		t.Fatalf("success = %v", err)
	}
	other := errors.New("not a process")
	if exitStatusOf(other) != other {
		t.Fatal("an unrelated error was changed")
	}
}

func TestNBDRequirement(t *testing.T) {
	if r := nbdRequirement(true, true); !r.OK {
		t.Errorf("a loaded nbd is not ok: %+v", r)
	}
	if r := nbdRequirement(false, true); r.OK || !r.Fixable {
		t.Errorf("a loadable nbd must be range's to load, not a blocker: %+v", r)
	}
	if r := nbdRequirement(false, false); r.OK || r.Fixable {
		t.Errorf("a kernel without nbd must block: %+v", r)
	}
}
