package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveInStaysInsideTheRoot(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"usr/bin", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(root, "usr/bin/dash"), []byte("dash"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/passwd"), []byte("root"), 0o644)
	// Ubuntu's merged /usr: /bin -> usr/bin, and /usr/bin/sh -> dash. An
	// absolute link starts again at the root, never at the host's.
	os.Symlink("usr/bin", filepath.Join(root, "bin"))
	os.Symlink("dash", filepath.Join(root, "usr/bin/sh"))
	os.Symlink("/etc/passwd", filepath.Join(root, "usr/bin/abs"))
	os.Symlink("../../../../../etc/passwd", filepath.Join(root, "usr/bin/up"))
	os.Symlink("loop", filepath.Join(root, "usr/bin/loop"))

	for path, want := range map[string]string{
		"/bin/sh": "usr/bin/dash", "/bin/abs": "etc/passwd", "/bin/up": "etc/passwd",
	} {
		got, err := resolveIn(root, path)
		if err != nil || got != filepath.Join(root, want) {
			t.Errorf("%s resolved to %q, %v; want %s", path, got, err, want)
		}
	}
	if _, err := resolveIn(root, "/bin/loop"); err == nil {
		t.Error("a link to itself resolved")
	}
	if _, err := resolveIn(root, "/bin/none"); err == nil {
		t.Error("a missing file resolved")
	}
}

func TestEnvironmentArch(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "bin"), 0o755)
	if got := environmentArch(root); got != "" {
		t.Errorf("no /bin/sh: %q", got)
	}
	// An x86-64 ELF header: e_machine 0x3e at byte 18.
	header := make([]byte, 64)
	copy(header, "\x7fELF\x02\x01")
	header[18] = 0x3e
	os.WriteFile(filepath.Join(root, "bin/sh"), header, 0o755)
	if got := environmentArch(root); got != "amd64" {
		t.Errorf("x86-64 /bin/sh: %q", got)
	}
}

func TestWithToolsFirst(t *testing.T) {
	image := map[string]string{"PATH": "/opt/bin:/bin", "LANG": "C"}
	got := withToolsFirst(image)
	if got["PATH"] != "/.range/bin:/opt/bin:/bin" || got["LANG"] != "C" || image["PATH"] != "/opt/bin:/bin" {
		t.Errorf("got %v, image now %v", got, image)
	}
	if got := withToolsFirst(nil); got["PATH"] != "/.range/bin:"+defaultPath {
		t.Errorf("no PATH: %v", got)
	}
}
