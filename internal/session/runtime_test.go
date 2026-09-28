package session

import (
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
		// macOS is supported through a Linux VM, so the shell must not refuse
		// outright; missing pieces are provisioned rather than fatal.
		if host.Name() != "lima" {
			t.Fatalf("runtime = %q, want lima on macOS", host.Name())
		}
		for _, req := range host.Requirements() {
			if !req.OK && !req.Fixable && req.What != "limactl" && req.What != "guest binary" {
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
