package oci

import (
	"archive/tar"
	"syscall"
	"testing"
)

// The shell is found through the image's own symlinks, not the builder's.
func TestOCITreeShellLookupFollowsImageSymlinks(t *testing.T) {
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "usr/bin/bash", Typeflag: tar.TypeReg, Mode: 0o755}, "elf"),
		tarEntry(t, &tar.Header{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "/usr/bin"}, ""),
	)
	tree, _, _ := buildTreeImage(t, layer)
	meta := environmentFor("x", ImageConfig{}, HostPlatform(), func(p string) bool {
		node, err := tree.lookup(p, true)
		return err == nil && node.Mode&syscall.S_IFMT == syscall.S_IFREG
	})
	if meta.Shell != "/bin/bash" {
		t.Errorf("shell = %q, want /bin/bash found through the /bin symlink", meta.Shell)
	}
}
