package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWantedReadsTheProject(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, ".tool-versions"), "nodejs 18.20.0\ngolang 1.20 # old\nruby 3.3.0\n")
	write(t, filepath.Join(root, ".python-version"), "pypy3.10\n")
	app := filepath.Join(root, "app")
	write(t, filepath.Join(app, "go.mod"), "module x\n\ngo 1.21\n\ntoolchain go1.22.5\n")
	write(t, filepath.Join(app, ".nvmrc"), "v20.11.0\n")
	write(t, filepath.Join(app, "rust-toolchain.toml"), "[toolchain]\nchannel = \"1.80.0\"\n")
	dir := filepath.Join(app, "cmd", "server")
	os.MkdirAll(dir, 0o755)

	got := Wanted("go@1.19 python@3.11-bookworm bad@ nope", dir)
	want := []Want{
		{"go", "1.19", UseVariable},
		{"python", "3.11-bookworm", UseVariable},
		// The nearest file wins: app's go.mod and .nvmrc over the root's
		// .tool-versions, whose ruby still counts.
		{"go", "1.22.5", filepath.Join(app, "go.mod")},
		{"node", "20.11.0", filepath.Join(app, ".nvmrc")},
		{"rustc", "1.80.0", filepath.Join(app, "rust-toolchain.toml")},
		{"ruby", "3.3.0", filepath.Join(root, ".tool-versions")},
	}
	// The root's own files are read last: keep only what this test wrote.
	var mine []Want
	for _, w := range got {
		if w.From == UseVariable || strings.HasPrefix(w.From, root) {
			mine = append(mine, w)
		}
	}
	if !reflect.DeepEqual(mine, want) {
		t.Errorf("got  %v\nwant %v", mine, want)
	}
}

func TestGoModWithoutToolchain(t *testing.T) {
	if v := goModVersion("module x\ngo 1.21.3\n"); v != "1.21.3" {
		t.Errorf("go.mod: %q", v)
	}
}

// finderOf answers Find from a table of commands and their usual images.
type finderOf map[string]string

func (f finderOf) Names(context.Context) ([]string, error) { return nil, nil }
func (f finderOf) Find(_ context.Context, command, version string) (string, string, error) {
	image, ok := f[command]
	if !ok {
		return "", "", errors.New("unknown")
	}
	return image, command, nil
}

func TestChooseVersionByImage(t *testing.T) {
	finder := finderOf{"go": "golang:1.23", "gofmt": "golang:1.23", "python3": "python:3.12-slim", "pip": "python:3.12-slim"}
	wants := []Want{{"python3", "3.11", "RANGE_USE"}, {"go", "1.22", "/src/go.mod"}}
	for command, want := range map[string]string{"gofmt": "1.22", "go": "1.22", "pip": "3.11", "jq": ""} {
		if got, _ := chooseVersion(context.Background(), finder, command, wants); got != want {
			t.Errorf("%s: %q, want %q", command, got, want)
		}
	}
	if _, from := chooseVersion(context.Background(), finder, "gofmt", wants); from != "/src/go.mod" {
		t.Errorf("from %q", from)
	}
}
