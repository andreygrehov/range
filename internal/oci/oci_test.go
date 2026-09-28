package oci

import (
	"archive/tar"
	"os"
	"testing"
	"time"
)

// The bug this replaces: unpacking without root made every file in the image
// belong to whoever ran the build, /etc/shadow and setuid binaries included.
// Ownership now comes from the tar headers and nothing else.
func TestOCITreeTakesOwnershipFromHeadersNotTheBuilder(t *testing.T) {
	stamp := time.Date(2025, 3, 4, 5, 6, 7, 800, time.UTC)
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: stamp}, ""),
		tarEntry(t, &tar.Header{Name: "etc/shadow", Typeflag: tar.TypeReg, Mode: 0o640, Uid: 0, Gid: 42, ModTime: stamp}, "root:*:1::::::\n"),
		tarEntry(t, &tar.Header{Name: "usr/bin/sudo", Typeflag: tar.TypeReg, Mode: 0o4755, ModTime: stamp}, "elf"),
		tarEntry(t, &tar.Header{Name: "home/app/data", Typeflag: tar.TypeReg, Mode: 0o600, Uid: 1234, Gid: 5678, ModTime: stamp}, "x"),
	)
	_, find, e := buildTreeImage(t, layer)
	shadow, ok := find("etc/shadow")
	if !ok {
		t.Fatal("etc/shadow missing")
	}
	if shadow.UID != 0 || shadow.GID != 42 || shadow.Mode&0o7777 != 0o640 {
		t.Errorf("etc/shadow = %d:%d %o, want 0:42 640", shadow.UID, shadow.GID, shadow.Mode&0o7777)
	}
	if string(e.Data(shadow)) != "root:*:1::::::\n" {
		t.Error("etc/shadow contents differ")
	}
	sudo, _ := find("usr/bin/sudo")
	if sudo.UID != 0 || sudo.Mode&0o7777 != 0o4755 {
		t.Errorf("sudo = uid %d mode %o, want root and setuid 4755", sudo.UID, sudo.Mode&0o7777)
	}
	data, _ := find("home/app/data")
	if data.UID != 1234 || data.GID != 5678 {
		t.Errorf("home/app/data = %d:%d, want 1234:5678", data.UID, data.GID)
	}
	if shadow.Mtime != stamp.Unix() {
		t.Errorf("mtime = %d, want %d", shadow.Mtime, stamp.Unix())
	}
	// Parents the layer never listed are root's, not the builder's.
	bin, _ := find("usr/bin")
	if bin.UID != 0 || bin.GID != 0 {
		t.Errorf("implicit directory usr/bin = %d:%d, want root", bin.UID, bin.GID)
	}
}

// Two names that differ only in case must stay two files, even when the build
// runs on a case-insensitive filesystem such as APFS.
func TestOCITreeKeepsNamesDifferingOnlyInCase(t *testing.T) {
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "include/xt_CONNMARK.h", Typeflag: tar.TypeReg, Mode: 0o644}, "upper"),
		tarEntry(t, &tar.Header{Name: "include/xt_connmark.h", Typeflag: tar.TypeReg, Mode: 0o644}, "lower"),
	)
	_, find, e := buildTreeImage(t, layer)
	upper, ok1 := find("include/xt_CONNMARK.h")
	lower, ok2 := find("include/xt_connmark.h")
	if !ok1 || !ok2 {
		t.Fatal("one of the two case-variant names is missing")
	}
	if string(e.Data(upper)) != "upper" || string(e.Data(lower)) != "lower" {
		t.Fatalf("contents folded together: %q, %q", e.Data(upper), e.Data(lower))
	}
}

func TestOCITreeWhiteouts(t *testing.T) {
	lower := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "gone", Typeflag: tar.TypeReg, Mode: 0o644}, "x"),
		tarEntry(t, &tar.Header{Name: "kept", Typeflag: tar.TypeReg, Mode: 0o644}, "x"),
		tarEntry(t, &tar.Header{Name: "opt/app/stale", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
	)
	upper := tarLayer(t,
		tarEntry(t, &tar.Header{Name: ".wh.gone", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
		// This layer's own entry is listed before the opaque marker and must
		// survive it; only what lower layers put there is hidden.
		tarEntry(t, &tar.Header{Name: "opt/app/fresh", Typeflag: tar.TypeReg, Mode: 0o644}, "new"),
		tarEntry(t, &tar.Header{Name: "opt/app/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
	)
	_, find, _ := buildTreeImage(t, lower, upper)
	if _, ok := find("gone"); ok {
		t.Error("a whited-out file survived")
	}
	if _, ok := find("kept"); !ok {
		t.Error("an untouched file was removed")
	}
	if _, ok := find("opt/app/stale"); ok {
		t.Error("the opaque marker left a lower-layer file")
	}
	if _, ok := find("opt/app/fresh"); !ok {
		t.Error("the opaque marker removed an entry from its own layer")
	}
}

// An opaque marker hides everything lower layers put under the directory, at
// any depth, even below a subdirectory this layer listed first; what this layer
// wrote survives, including files under a directory it never declared.
func TestOCITreeOpaqueWhiteoutIsRecursive(t *testing.T) {
	lower := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "opq2/-a/lowerfile", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
		tarEntry(t, &tar.Header{Name: "opq3/sub/old", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
		tarEntry(t, &tar.Header{Name: "opq3/gone/deep/file", Typeflag: tar.TypeReg, Mode: 0o644}, "old"),
	)
	upper := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "opq2/-a/", Typeflag: tar.TypeDir, Mode: 0o700}, ""),
		tarEntry(t, &tar.Header{Name: "opq2/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
		tarEntry(t, &tar.Header{Name: "opq3/sub/new", Typeflag: tar.TypeReg, Mode: 0o644}, "new"),
		tarEntry(t, &tar.Header{Name: "opq3/.wh..wh..opq", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
	)
	_, find, _ := buildTreeImage(t, lower, upper)
	for p, want := range map[string]bool{
		"opq2/-a":           true,
		"opq2/-a/lowerfile": false,
		"opq3/sub/new":      true,
		"opq3/sub/old":      false,
		"opq3/gone":         false,
	} {
		if _, ok := find(p); ok != want {
			t.Errorf("%s present = %v, want %v", p, ok, want)
		}
	}
	if a, _ := find("opq2/-a"); a.Mode&0o777 != 0o700 {
		t.Errorf("opq2/-a mode %o, want the upper layer's 700", a.Mode&0o777)
	}
}

// Nothing a layer contains can put a byte outside the blob store.
func TestOCITreeWritesOnlyIntoTheBlobStore(t *testing.T) {
	outside := t.TempDir()
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside}, ""),
		tarEntry(t, &tar.Header{Name: "escape/owned", Typeflag: tar.TypeReg, Mode: 0o644}, "owned"),
		tarEntry(t, &tar.Header{Name: "../../escape2", Typeflag: tar.TypeReg, Mode: 0o644}, "owned"),
	)
	buildTreeImage(t, layer)
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("a layer wrote outside the store: %v", entries)
	}
}
