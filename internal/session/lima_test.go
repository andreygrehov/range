package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/image"
	"github.com/andreygrehov/range/internal/rangetest"
)

func TestLimaBuildRequestsRawImage(t *testing.T) {
	dir := t.TempDir()
	guest := filepath.Join(dir, "range-linux")
	if err := os.WriteFile(guest, []byte("test guest binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RANGE_GUEST_BINARY", guest)
	// Simulate an already provisioned VM and check the command at the guest
	// boundary. The host must receive raw bytes so it can pack them once.
	shim := `#!/bin/sh
if [ "$1" = list ]; then
  printf 'test-vm\tRunning\n'
  exit 0
fi
shift 4
if [ "$1" = sudo ] && [ "$2" = /usr/local/bin/range ]; then
  shift 2
  [ "$1" = build ] || exit 21
  shift
  format=
  output=
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --format) format=$2; shift 2 ;;
      --output) output=$2; shift 2 ;;
      --size|--from-oci) shift 2 ;;
      *) shift ;;
    esac
  done
  [ "$format" = raw ] || exit 22
  printf 'Pulling test image\n  layer 1/1\n' >&2
  printf 'raw filesystem bytes' > "$output"
  printf 'Built %s\n\nUse it with:\n  range shell %s\n' "$output" "$output"
fi
`
	if err := os.WriteFile(filepath.Join(dir, "limactl"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.Mkdir(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, fromOCI := range []string{"golang:1.23", ""} {
		t.Run("from-oci="+fromOCI, func(t *testing.T) {
			readOutput, _ := rangetest.CaptureStdout(t)
			stderr := os.Stderr
			os.Stderr = os.Stdout
			t.Cleanup(func() { os.Stderr = stderr })
			output := filepath.Join(t.TempDir(), "image.raw")
			req := image.BuildRequest{Rootfs: rootfs, FromOCI: fromOCI, Size: 64 << 20, Output: output}
			if err := (lima{instance: "test-vm"}).BuildImage(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(output)
			if err != nil || string(got) != "raw filesystem bytes" {
				t.Fatalf("built image = %q, %v", got, err)
			}
			log := readOutput()
			if strings.Contains(log, "Built ") || strings.Contains(log, "range shell") {
				t.Fatalf("guest advertised a temporary artifact: %s", log)
			}
			if !strings.Contains(log, "Pulling test image") || !strings.Contains(log, "layer 1/1") {
				t.Fatalf("guest progress was lost: %s", log)
			}
		})
	}
}

// "No daemon" is claimed only where it is true; on macOS the VM is long-lived.
func TestNotNeededIsHonestPerRuntime(t *testing.T) {
	if !strings.Contains(NotNeeded(Native{}), "a daemon") {
		t.Error("native Linux should claim it needs no daemon")
	}
	if strings.Contains(NotNeeded(lima{}), "a daemon") {
		t.Error("the Lima runtime must not claim to need no daemon")
	}
}

func TestGuestBinarySourceHonoursExplicitPath(t *testing.T) {
	t.Setenv("RANGE_GUEST_BINARY", filepath.Join(t.TempDir(), "absent"))
	if _, err := guestBinarySource(); err == nil {
		t.Fatal("expected an error for a missing RANGE_GUEST_BINARY")
	}
	binary := filepath.Join(t.TempDir(), "range-linux")
	if err := os.WriteFile(binary, []byte("elf"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RANGE_GUEST_BINARY", binary)
	got, err := guestBinarySource()
	if err != nil || got != binary {
		t.Fatalf("guestBinarySource() = %q, %v", got, err)
	}
}

func TestFileChecksumIsStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("range"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, err := fileChecksum(path)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := fileChecksum(path)
	if a != b || len(a) != 16 {
		t.Fatalf("checksum = %q / %q", a, b)
	}
}
