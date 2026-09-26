package session

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/environment"
	"github.com/andreygrehov/range/internal/oci"
)

func TestChildEnvIsSortedAndComplete(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	env := childEnv(sessionConfig{
		Hostname:    "range",
		Environment: map[string]string{"ZED": "1", "GOPATH": "/root/go", "ALPHA": "a"},
	})
	joined := strings.Join(env, " ")
	for _, want := range []string{"HOME=/root", "HOSTNAME=range", "TERM=xterm-256color", "GOPATH=/root/go"} {
		if !strings.Contains(joined, want) {
			t.Errorf("child environment is missing %q: %v", want, env)
		}
	}
	if !strings.HasPrefix(env[0], "PATH=") {
		t.Fatalf("first entry = %q, want PATH", env[0])
	}
	// The image-supplied variables must be in a stable order.
	custom := env[len(env)-3:]
	if fmt.Sprint(custom) != fmt.Sprint([]string{"ALPHA=a", "GOPATH=/root/go", "ZED=1"}) {
		t.Fatalf("image environment is not sorted: %v", custom)
	}
}

// syscall.Exec takes a path, so "range run <uri> -- go version" only works if
// the bare name is resolved against the environment's own PATH first.
func TestLookPathInEnvironment(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "usr", "local", "go", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(bin, "go")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=/root", "PATH=" + filepath.Join(root, "nowhere") + ":" + bin}

	got, err := lookPathInEnvironment("go", env)
	if err != nil {
		t.Fatalf("lookPathInEnvironment: %v", err)
	}
	if got != tool {
		t.Errorf("resolved to %q, want %q", got, tool)
	}
	// A path is taken as given, so nothing is second-guessed.
	if got, err := lookPathInEnvironment("/bin/bash", env); err != nil || got != "/bin/bash" {
		t.Errorf("an explicit path was rewritten to %q (%v)", got, err)
	}
	// The error has to name PATH, or the user cannot tell why it failed.
	_, err = lookPathInEnvironment("definitely-absent", env)
	if err == nil {
		t.Fatal("a missing command resolved anyway")
	}
	if !strings.Contains(err.Error(), "PATH is") {
		t.Errorf("error should show the PATH searched: %v", err)
	}
	// A directory on PATH with the right name is not a program.
	if err := os.MkdirAll(filepath.Join(bin, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := lookPathInEnvironment("adir", env); err == nil {
		t.Error("a directory was resolved as a program")
	}
	// Neither is a file without an executable bit.
	if err := os.WriteFile(filepath.Join(bin, "data"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := lookPathInEnvironment("data", env); err == nil {
		t.Error("a non-executable file was resolved as a program")
	}
	// With no PATH at all the usual system directories are searched.
	if _, err := lookPathInEnvironment("definitely-absent", []string{"HOME=/root"}); err == nil {
		t.Error("expected a miss with the default PATH")
	}
}

// A cross-architecture artifact must say so, not fail with "exec format error".
func TestCheckExecutableArch(t *testing.T) {
	dir := t.TempDir()
	elf := func(name string, machine uint16, class byte) string {
		header := make([]byte, 64)
		copy(header, "\x7fELF")
		header[4], header[5] = class, 1 // 64-bit, little-endian
		binary.LittleEndian.PutUint16(header[18:20], machine)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, header, 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	here := map[string]uint16{"amd64": 0x3e, "arm64": 0xb7, "386": 0x03, "riscv64": 0xf3}
	foreign := uint16(0x3e) // x86-64
	if runtime.GOARCH == "amd64" {
		foreign = 0xb7 // aarch64
	}
	err := checkExecutableArch(elf("foreign", foreign, 2))
	if err == nil {
		t.Fatal("a binary for another architecture was accepted")
	}
	if !strings.Contains(err.Error(), runtime.GOARCH) {
		t.Errorf("error should name the environment's architecture: %v", err)
	}
	if machine, ok := here[runtime.GOARCH]; ok {
		if err := checkExecutableArch(elf("native", machine, 2)); err != nil {
			t.Errorf("a native binary was rejected: %v", err)
		}
	}
	// A shell script has no ELF header and must be left to exec.
	script := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := checkExecutableArch(script); err != nil {
		t.Errorf("a script was rejected: %v", err)
	}
	if err := checkExecutableArch(filepath.Join(dir, "absent")); err == nil {
		t.Error("a missing program was accepted")
	}
}

func TestOCIEnvironmentBecomesArtifactMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", "bash"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	var config oci.ImageConfig
	config.Config.Env = []string{"PATH=/usr/local/go/bin:/usr/bin", "GOPATH=/go"}
	config.Config.WorkingDir = "/go/src"
	if err := oci.WriteEnvironment(dir, "golang:1.23", config, oci.HostPlatform()); err != nil {
		t.Fatal(err)
	}
	meta := environment.ReadMetadata(dir)
	if meta.Environment["PATH"] != "/usr/local/go/bin:/usr/bin" {
		t.Fatalf("PATH lost: %+v", meta.Environment)
	}
	if meta.Workdir != "/go/src" || meta.Shell != "/bin/bash" || meta.Name != "golang-1.23" {
		t.Fatalf("metadata = %+v", meta)
	}
	// The image's PATH must survive into the child, not be shadowed by defaults.
	env := childEnv(sessionConfig{Hostname: "range", Environment: meta.Environment})
	joined := strings.Join(env, " ")
	if !strings.Contains(joined, "PATH=/usr/local/go/bin:/usr/bin") {
		t.Fatalf("image PATH did not reach the child: %v", env)
	}
	if strings.Count(joined, "PATH=") != 2 { // PATH and GOPATH
		t.Fatalf("PATH declared more than once: %v", env)
	}
}
