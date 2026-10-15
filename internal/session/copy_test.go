package session

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// copyTree copies src to dest as range cp does, through a tar stream.
func copyTree(t *testing.T, src, dest string) error {
	t.Helper()
	spec, err := SpecOf(src, false)
	if err != nil {
		return err
	}
	plan, err := PlanCopy(spec, dest)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := WriteTar(&buf, src, spec); err != nil {
		t.Fatal(err)
	}
	return ExtractTar(&buf, plan, nil)
}

func lexists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

func TestCopyAsDockerDoes(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "app", "sub"), 0o755)
	os.WriteFile(filepath.Join(src, "app", "main.go"), []byte("package main"), 0o644)
	os.WriteFile(filepath.Join(src, "app", "sub", "x.txt"), []byte("x"), 0o600)
	os.Symlink("main.go", filepath.Join(src, "app", "link"))
	os.WriteFile(filepath.Join(src, "file.txt"), []byte("hello"), 0o644)

	dst := t.TempDir()
	os.Mkdir(filepath.Join(dst, "into"), 0o755)
	for _, c := range []struct {
		src, dest string
		want      []string
	}{
		{"file.txt", "into", []string{"into/file.txt"}},                                      // a file into a directory
		{"file.txt", "renamed.txt", []string{"renamed.txt"}},                                 // a file as a new name
		{"app", "into", []string{"into/app/main.go", "into/app/sub/x.txt", "into/app/link"}}, // a directory into one
		{"app", "fresh", []string{"fresh/main.go", "fresh/sub/x.txt"}},                       // a directory as a new one
		{"app/.", "into", []string{"into/main.go", "into/sub/x.txt"}},                        // its contents
	} {
		if err := copyTree(t, src+"/"+c.src, filepath.Join(dst, c.dest)); err != nil {
			t.Fatalf("%s -> %s: %v", c.src, c.dest, err)
		}
		for _, w := range c.want {
			if !lexists(t, filepath.Join(dst, w)) {
				t.Errorf("%s -> %s: no %s", c.src, c.dest, w)
			}
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dst, "into", "sub", "x.txt")); string(data) != "x" {
		t.Errorf("x.txt holds %q", data)
	}
	if info, _ := os.Stat(filepath.Join(dst, "into", "sub", "x.txt")); info.Mode().Perm() != 0o600 {
		t.Errorf("x.txt mode %v", info.Mode())
	}
	if target, _ := os.Readlink(filepath.Join(dst, "into", "app", "link")); target != "main.go" {
		t.Errorf("link -> %q", target)
	}
	// A directory cannot go over a file, nor anything into a missing parent.
	if err := copyTree(t, filepath.Join(src, "app"), filepath.Join(dst, "renamed.txt")); err == nil {
		t.Error("a directory went over a file")
	}
	if err := copyTree(t, filepath.Join(src, "file.txt"), filepath.Join(dst, "no", "such")); err == nil {
		t.Error("a file went into a missing directory")
	}
}

// A stream cannot write outside its destination: not with "..", nor
// through a link it made first.
func TestExtractStaysInside(t *testing.T) {
	outside := t.TempDir()
	for name, entries := range map[string][]tar.Header{
		"dotdot": {{Name: "x/../../evil", Typeflag: tar.TypeReg, Mode: 0o644}},
		"link": {
			{Name: "x/", Typeflag: tar.TypeDir, Mode: 0o755},
			{Name: "x/out", Typeflag: tar.TypeSymlink, Linkname: outside},
			{Name: "x/out/evil", Typeflag: tar.TypeReg, Mode: 0o644},
		},
	} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for _, h := range entries {
			tw.WriteHeader(&h)
		}
		tw.Close()
		dest := t.TempDir()
		ExtractTar(&buf, CopyPlan{dest, "x"}, nil)
		if lexists(t, filepath.Join(outside, "evil")) || lexists(t, filepath.Join(filepath.Dir(dest), "evil")) {
			t.Errorf("%s: wrote outside", name)
		}
	}
}
