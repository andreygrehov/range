package session

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/andreygrehov/range/internal/commands"
)

// A command in a session runs the version that was asked for, from the image
// of that tag: go@1.22 runs from golang:1.22. Without one, the version comes
// from RANGE_USE, then from the project's own files, nearest first, then from
// the catalog's map. A version applies to its command's whole image, so
// RANGE_USE=go@1.22 makes gofmt 1.22 too.

// Want is a version asked for a command's image, and where it was asked.
type Want struct {
	Command string `json:"command"`
	Version string `json:"version"`
	From    string `json:"from"`
}

// UseVariable lists versions for the shell and what it starts:
// RANGE_USE="go@1.22 python@3.11".
const UseVariable = "RANGE_USE"

// Wanted lists the versions asked for, best first: use, the value of
// RANGE_USE, then the project files from dir up.
func Wanted(use, dir string) []Want {
	var wants []Want
	for _, field := range strings.FieldsFunc(use, func(r rune) bool { return r == ' ' || r == ',' }) {
		command, version, ok := strings.Cut(field, "@")
		if ok && command != "" && validTag.MatchString(version) {
			wants = append(wants, Want{command, version, UseVariable})
		}
	}
	return append(wants, projectVersions(dir)...)
}

// validTag is a tag as a registry takes it.
var validTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

// numeric is a version a project file names by number: 1.22, v20.11.0. Names
// such as lts/iron, stable or system are a version manager's own, and left to
// the catalog's map.
var numeric = regexp.MustCompile(`^v?([0-9]+(\.[0-9]+)*)$`)

// projectFiles are the files that name a version, and the command whose
// image they choose. .tool-versions names many.
var projectFiles = []struct {
	name    string
	command string
	read    func(data string) string
}{
	{"go.mod", "go", goModVersion},
	{".nvmrc", "node", firstLine},
	{".node-version", "node", firstLine},
	{".python-version", "python3", firstLine},
	{".ruby-version", "ruby", func(s string) string { return strings.TrimPrefix(firstLine(s), "ruby-") }},
	{"rust-toolchain.toml", "rustc", rustChannel},
	{"rust-toolchain", "rustc", firstLine},
}

// asdfCommands names a command of each asdf plugin whose name is not one.
var asdfCommands = map[string]string{
	"golang": "go", "nodejs": "node", "python": "python3", "rust": "rustc", "erlang": "erl",
}

// projectVersions reads the versions a project names, from dir up to the
// root. The nearest file for a command wins.
func projectVersions(dir string) []Want {
	var wants []Want
	seen := map[string]bool{}
	add := func(command, version, from string) {
		m := numeric.FindStringSubmatch(strings.TrimSpace(version))
		if m == nil || seen[command] {
			return
		}
		seen[command] = true
		wants = append(wants, Want{command, m[1], from})
	}
	for dir = filepath.Clean(dir); ; dir = filepath.Dir(dir) {
		for _, f := range projectFiles {
			path := filepath.Join(dir, f.name)
			if data, err := readSmall(path); err == nil {
				add(f.command, f.read(data), path)
			}
		}
		path := filepath.Join(dir, ".tool-versions")
		if data, err := readSmall(path); err == nil {
			for _, line := range strings.Split(data, "\n") {
				fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
				if len(fields) < 2 {
					continue
				}
				command := fields[0]
				if c, ok := asdfCommands[command]; ok {
					command = c
				}
				add(command, fields[1], path)
			}
		}
		if dir == filepath.Dir(dir) {
			return wants
		}
	}
}

// readSmall reads a project file, which is small: a large one is not one.
func readSmall(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", os.ErrNotExist
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// goModVersion is the toolchain a go.mod asks for, or else its go version.
func goModVersion(s string) string {
	var goLine string
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 2 && fields[0] == "toolchain":
			return strings.TrimPrefix(fields[1], "go")
		case len(fields) == 2 && fields[0] == "go" && goLine == "":
			goLine = fields[1]
		}
	}
	return goLine
}

// rustChannel is the channel of a rust-toolchain.toml: channel = "1.80.0".
func rustChannel(s string) string {
	for _, line := range strings.Split(s, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "channel" {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

// chooseVersion picks the version for command among wants: the first whose
// command runs from the same image. It asks finder for each command's usual
// image, the map's own answer, to tell.
func chooseVersion(ctx context.Context, finder Finder, command string, wants []Want) (version, from string) {
	if len(wants) == 0 {
		return "", ""
	}
	image, _, err := finder.Find(ctx, command, "")
	if err != nil {
		return "", ""
	}
	repo := commands.Repository(image)
	for _, w := range wants {
		if w.Command == command {
			return w.Version, w.From
		}
		if other, _, err := finder.Find(ctx, w.Command, ""); err == nil && commands.Repository(other) == repo {
			return w.Version, w.From
		}
	}
	return "", ""
}
