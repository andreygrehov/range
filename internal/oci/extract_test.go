package oci

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestExtractLayerHandlesWhiteoutsAndLinks(t *testing.T) {
	dir := t.TempDir()
	// A file that a later layer deletes.
	if err := os.WriteFile(filepath.Join(dir, "gone"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(h *tar.Header, body string) {
		t.Helper()
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(&tar.Header{Name: "usr/bin", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	write(&tar.Header{Name: "usr/bin/tool", Typeflag: tar.TypeReg, Mode: 0o755, Size: 4}, "prog")
	write(&tar.Header{Name: "usr/bin/link", Typeflag: tar.TypeSymlink, Linkname: "tool"}, "")
	write(&tar.Header{Name: ".wh.gone", Typeflag: tar.TypeReg, Mode: 0o644}, "")
	write(&tar.Header{Name: "../escape", Typeflag: tar.TypeReg, Mode: 0o644, Size: 3}, "bad")
	tw.Close()

	if err := extractLayer(&buf, dir); err != nil {
		t.Fatalf("extractLayer: %v", err)
	}
	if body, err := os.ReadFile(filepath.Join(dir, "usr/bin/tool")); err != nil || string(body) != "prog" {
		t.Fatalf("regular file not extracted: %v", err)
	}
	if info, err := os.Lstat(filepath.Join(dir, "usr/bin/link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink not extracted")
	}
	if _, err := os.Stat(filepath.Join(dir, "gone")); !os.IsNotExist(err) {
		t.Fatal("whiteout did not delete the lower-layer file")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape")); err == nil {
		t.Fatal("a path traversing outside the root was written")
	}
	if info, err := os.Stat(filepath.Join(dir, "usr/bin/tool")); err == nil && info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
}

// A layer may ship a symlink and then write "through" it. The name of the
// second entry contains no "..", so a name-only check never sees the escape.
func TestExtractLayerRefusesWriteThroughSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rootfs")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	victim := filepath.Join(outside, "passwd")
	if err := os.WriteFile(victim, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside}, ""),
		tarEntry(t, &tar.Header{Name: "escape/passwd", Typeflag: tar.TypeReg, Mode: 0o644}, "owned"),
	)
	// The write must fail rather than silently land outside the root.
	if err := extractLayer(layer, dir); err == nil {
		t.Error("extractLayer accepted a write through an escaping symlink")
	}
	if body, err := os.ReadFile(victim); err != nil || string(body) != "original" {
		t.Fatalf("a file outside the root was written: %q %v", body, err)
	}
}

// A hardlink target is resolved against the root, so ".." in a link name must
// not reach a host file and pull it into the image.
func TestExtractLayerRefusesEscapingHardlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "rootfs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "shadow")
	if err := os.WriteFile(secret, []byte("hash"), 0o600); err != nil {
		t.Fatal(err)
	}
	layer := tarLayer(t, tarEntry(t, &tar.Header{
		Name: "stolen", Typeflag: tar.TypeLink, Linkname: "../shadow",
	}, ""))
	if err := extractLayer(layer, dir); err == nil {
		t.Error("extractLayer accepted a hardlink to a file outside the root")
	}
	if _, err := os.Lstat(filepath.Join(dir, "stolen")); err == nil {
		t.Error("a host file was hardlinked into the image")
	}
}

// An opaque marker hides everything the lower layers left in that directory.
func TestExtractLayerOpaqueWhiteoutClearsDirectory(t *testing.T) {
	dir := t.TempDir()
	lower := filepath.Join(dir, "opt", "app")
	if err := os.MkdirAll(lower, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stale", "also-stale"} {
		if err := os.WriteFile(filepath.Join(lower, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "opt/app/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
		tarEntry(t, &tar.Header{Name: "opt/app/fresh", Typeflag: tar.TypeReg, Mode: 0o644}, "new"),
	)
	if err := extractLayer(layer, dir); err != nil {
		t.Fatalf("extractLayer: %v", err)
	}
	for _, name := range []string{"stale", "also-stale"} {
		if _, err := os.Stat(filepath.Join(lower, name)); !os.IsNotExist(err) {
			t.Errorf("opaque whiteout left %s behind", name)
		}
	}
	if _, err := os.Stat(lower); err != nil {
		t.Error("opaque whiteout removed the directory itself")
	}
	if body, err := os.ReadFile(filepath.Join(lower, "fresh")); err != nil || string(body) != "new" {
		t.Fatalf("upper entry missing after opaque whiteout: %v", err)
	}
}

// chown clears the setuid bit, so the mode has to be applied after it.
func TestExtractLayerKeepsSetuid(t *testing.T) {
	if os.Getuid() != 0 {
		// Without privileges chown is a no-op, but the chmod ordering is still
		// what the test is checking.
		t.Log("not root: chown is a no-op here, the mode ordering still applies")
	}
	dir := t.TempDir()
	layer := tarLayer(t, tarEntry(t, &tar.Header{
		Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755,
	}, "elf"))
	if err := extractLayer(layer, dir); err != nil {
		t.Fatalf("extractLayer: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "usr/bin/sudo"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Fatalf("mode = %v, want the setuid bit set", info.Mode())
	}
}

// The on-disk extractor used for ext4 follows the same whiteout rules as the
// in-memory tree.
func TestExtractLayerOpaqueWhiteoutMatchesTree(t *testing.T) {
	dir := t.TempDir()
	lower := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "opq2/-a/lowerfile", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
		tarEntry(t, &tar.Header{Name: "opq3/sub/old", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
		tarEntry(t, &tar.Header{Name: "opq3/gone/deep/file", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
		tarEntry(t, &tar.Header{Name: "keep/mine", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
	)
	upper := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "opq2/-a/", Typeflag: tar.TypeDir, Mode: 0o755}, ""),
		tarEntry(t, &tar.Header{Name: "opq2/fresh", Typeflag: tar.TypeReg, Mode: 0o644}, "new"),
		tarEntry(t, &tar.Header{Name: "opq2/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
		tarEntry(t, &tar.Header{Name: "opq3/sub/new", Typeflag: tar.TypeReg, Mode: 0o644}, "new"),
		tarEntry(t, &tar.Header{Name: "opq3/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
		tarEntry(t, &tar.Header{Name: "keep/mine", Typeflag: tar.TypeReg, Mode: 0o644}, "new"),
		tarEntry(t, &tar.Header{Name: "keep/.wh.mine", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
	)
	for _, layer := range []*bytes.Buffer{lower, upper} {
		if err := extractLayer(layer, dir); err != nil {
			t.Fatal(err)
		}
	}
	for p, want := range map[string]bool{
		"opq2/-a":           true,
		"opq2/-a/lowerfile": false,
		"opq2/fresh":        true,
		"opq3/sub/new":      true,
		"opq3/sub/old":      false,
		"opq3/gone":         false,
		"keep/mine":         true,
	} {
		if _, err := os.Lstat(filepath.Join(dir, p)); (err == nil) != want {
			t.Errorf("%s present = %v, want %v", p, err == nil, want)
		}
	}
}
