package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMount(t *testing.T) {
	for _, tc := range []struct{ spec, uri, target string }{
		{"hf://openai-community/gpt2:/models", "hf://openai-community/gpt2", "/models"},
		{"hf://org/m@abc:/opt/model", "hf://org/m@abc", "/opt/model"},
		{"s3://bucket/data.range:/data", "s3://bucket/data.range", "/data"},
		{"https://host:8080/x.range:/x", "https://host:8080/x.range", "/x"},
		{"/local/file.img:/mnt/file", "/local/file.img", "/mnt/file"},
	} {
		uri, target, err := ParseMount(tc.spec)
		if err != nil || uri != tc.uri || target != tc.target {
			t.Errorf("%s: got %q %q %v", tc.spec, uri, target, err)
		}
	}
	for _, bad := range []string{
		"hf://org/model", "https://host:8080/x", "hf://org/model:/", "hf://org/model:/a/../b",
		"hf://org/model:/a/", "hf://org/model:models", ":/models",
	} {
		if _, _, err := ParseMount(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestMakeDirInCreatesTheTarget(t *testing.T) {
	root := t.TempDir()
	dir, err := makeDirIn(root, "/opt/models")
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(root, "opt", "models") {
		t.Errorf("got %s", dir)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("%s not created: %v", dir, err)
	}
	if _, err := makeDirIn(root, "/opt/models"); err != nil {
		t.Errorf("an existing directory was refused: %v", err)
	}
}

// An environment decides what its paths are. A /models that links out of the
// root must never place the mount on the host.
func TestMakeDirInRefusesSymlinks(t *testing.T) {
	root, host := t.TempDir(), t.TempDir()
	if err := os.Symlink(host, filepath.Join(root, "models")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/", filepath.Join(root, "opt")); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/models", "/models/inner", "/opt/x"} {
		_, err := makeDirIn(root, target)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("%s: got %v, want a refusal over the symlink", target, err)
		}
	}
	if entries, _ := os.ReadDir(host); len(entries) != 0 {
		t.Errorf("something was created outside the root: %v", entries)
	}
}

func TestMakeDirInRefusesAFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "models"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := makeDirIn(root, "/models"); err == nil {
		t.Error("a file was accepted as a mount point")
	}
}
