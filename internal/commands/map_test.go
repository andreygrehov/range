package commands

import (
	"fmt"
	"testing"

	"github.com/andreygrehov/range/internal/oci"
)

// image is an image of the test, on amd64, whose layers are named by letter.
func image(name string, layers string, commands map[string]int, os ...string) oci.ImageCommands {
	img := oci.ImageCommands{Image: name, Platform: "linux/amd64", Commands: commands, OS: os}
	for _, l := range layers {
		img.Layers = append(img.Layers, "sha256:"+string(l))
	}
	return img
}

func repos(owners []Owner) string {
	var names []string
	for _, o := range owners {
		names = append(names, o.Repo)
	}
	return fmt.Sprint(names)
}

func TestBuild(t *testing.T) {
	images := []oci.ImageCommands{
		// debian is the OS: ls in its bottom layer.
		image("debian:12", "d", map[string]int{"ls": 0}, "ls"),
		// buildpack-deps adds git and curl on debian.
		image("buildpack-deps:scm", "dg", map[string]int{"ls": 0, "git": 1, "curl": 1}, "ls"),
		// python adds python3 and pip in one layer on buildpack-deps, and
		// updates ls in another.
		image("python:3.12", "dgpu", map[string]int{"ls": 3, "git": 1, "curl": 1, "python3": 2, "pip": 2}, "ls"),
		// haxe ships its own python3 next to haxe.
		image("haxe:4", "dh", map[string]int{"ls": 0, "haxe": 1, "python3": 1}, "ls"),
		// jq is built from scratch: its entrypoint is its command.
		{Image: "ghcr.io/jqlang/jq:1.7", Platform: "linux/arm64", Layers: []string{"sha256:j"},
			Commands: map[string]int{"jq": 0}, Entrypoint: "jq"},
		// mongo carries a jq of its own.
		image("mongo:7", "dm", map[string]int{"ls": 0, "jq": 1, "mongod": 1}, "ls"),
		// erlang is named for erl.
		image("erlang:27", "de", map[string]int{"ls": 0, "erl": 1}, "ls"),
	}
	pulls := map[string]int64{"python": 9e9, "haxe": 1e6, "mongo": 2e9}
	m := Build(images, pulls, Pins{Skip: map[string]bool{"mongod": true}})

	for command, want := range map[string]string{
		"python3": "[python haxe]",             // named first
		"pip":     "[python]",                  // in python's own layer
		"git":     "[buildpack-deps]",          // inherited by python, still its parent's
		"jq":      "[ghcr.io/jqlang/jq mongo]", // the entrypoint
		"erl":     "[erlang]",
		"ls":      "[]", // the OS's, though python updated it
		"mongod":  "[]", // skipped
	} {
		if got := repos(m[command]); got != want {
			t.Errorf("%s: got %s, want %s", command, got, want)
		}
	}
	if p := fmt.Sprint(m["jq"][0].Platforms); p != "[linux/arm64]" {
		t.Errorf("jq platforms = %s", p)
	}

	// A pin puts its repository first.
	m = Build(images, pulls, Pins{Repo: map[string]string{"python3": "haxe"}})
	if got := repos(m["python3"]); got != "[haxe python]" {
		t.Errorf("pinned python3: %s", got)
	}
}

func TestNamedFor(t *testing.T) {
	for _, tc := range []struct {
		command, repo string
		want          int
	}{
		{"python", "python", exactName},
		{"python3.12", "python", exactName},
		{"terraform", "hashicorp/terraform", exactName},
		{"redis-cli", "redis", family},
		{"pg_dump", "pg", family},
		{"erl", "erlang", family},
		{"go", "golang", family},
		// Another tool whose name starts with the command's.
		{"cargo", "lukemathwalker/cargo-chef", notNamed},
		{"terraform", "ljfranklin/terraform-resource", notNamed},
		{"node", "nodered/node-red", notNamed},
		{"pip", "python", notNamed},
	} {
		if got := namedFor(tc.command, tc.repo, false); got != tc.want {
			t.Errorf("namedFor(%s, %s) = %d, want %d", tc.command, tc.repo, got, tc.want)
		}
	}
	if namedFor("jq", "ghcr.io/jqlang/jq", true) != exactName || namedFor("ffmpegwrapper.sh", "linuxserver/ffmpeg", true) != exactName {
		t.Error("an entrypoint is the image's own command")
	}
}

func TestAHomeRanksWhatItBrings(t *testing.T) {
	images := []oci.ImageCommands{
		image("texlive/texlive:latest", "dt", map[string]int{"pdflatex": 1, "bibtex": 1}),
		image("kasmweb/desktop:1", "dk", map[string]int{"pdflatex": 1, "bibtex": 1}),
	}
	pulls := map[string]int64{"kasmweb/desktop": 9e9, "texlive/texlive": 1e6}
	if got := repos(Build(images, pulls, Pins{})["pdflatex"]); got != "[kasmweb/desktop texlive/texlive]" {
		t.Fatalf("without a home, pulls decide: %s", got)
	}
	m := Build(images, pulls, Pins{Home: map[string]bool{"texlive/texlive": true}})
	for _, c := range []string{"pdflatex", "bibtex"} {
		if got := repos(m[c]); got != "[texlive/texlive kasmweb/desktop]" {
			t.Errorf("%s with texlive as its home: %s", c, got)
		}
	}
}

func TestRepository(t *testing.T) {
	for in, want := range map[string]string{
		"python:3.12": "python", "ghcr.io/org/app:1.2": "ghcr.io/org/app",
		"localhost:5000/app": "localhost:5000/app", "app@sha256:ab": "app", "app": "app",
	} {
		if got := Repository(in); got != want {
			t.Errorf("Repository(%q) = %q, want %q", in, got, want)
		}
	}
}
