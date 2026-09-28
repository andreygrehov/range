package erofs_test

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func erofsTestTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "rootfs")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "usr/bin"), 0o755))
	must(os.MkdirAll(filepath.Join(root, "empty"), 0o700))
	must(os.MkdirAll(filepath.Join(root, "crowded"), 0o755))
	must(os.WriteFile(filepath.Join(root, "usr/bin/tool"), []byte("#!/bin/sh\necho tool\n"), 0o755))
	must(os.WriteFile(filepath.Join(root, "zero-length"), nil, 0o644))
	big := make([]byte, 1<<20+300<<10) // over a chunk: must be chunk-aligned
	rand.New(rand.NewSource(3)).Read(big)
	must(os.WriteFile(filepath.Join(root, "usr/bin/big"), big, 0o755))
	must(os.Link(filepath.Join(root, "usr/bin/tool"), filepath.Join(root, "usr/bin/tool-link")))
	must(os.Symlink("usr/bin/tool", filepath.Join(root, "shortcut")))
	// Enough long names to need several directory blocks.
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("entry-%03d-%s", i, strings.Repeat("x", 40))
		must(os.WriteFile(filepath.Join(root, "crowded", name), []byte(name), 0o644))
	}
	stamp := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if info != nil && info.Mode()&os.ModeSymlink == 0 {
			os.Chtimes(path, stamp, stamp)
		}
		return nil
	})
	return root
}
