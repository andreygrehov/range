package main

import (
	"testing"

	"github.com/andreygrehov/range/internal/commands"
)

func TestPickOwner(t *testing.T) {
	owners := []commands.Owner{
		{Repo: "nvidia/cuda", Platforms: []string{"linux/amd64"}, Tag: "12.8.0-devel-ubuntu24.04"},
		{Repo: "python", Platforms: []string{"linux/amd64", "linux/arm64"}, Tag: "3.12"},
		{Repo: "ghcr.io/jqlang/jq", Platforms: []string{"linux/arm64"}, Tag: "latest", Path: "/jq"},
	}
	for _, tc := range []struct {
		platform, name, version string
		image, program          string
		official                bool
	}{
		// The first owner built for the platform, with the tag the catalog read.
		{"linux/amd64", "nvcc", "", "nvidia/cuda:12.8.0-devel-ubuntu24.04", "nvcc", false},
		{"linux/arm64", "python3", "", "python:3.12", "python3", true},
		// A version is a tag.
		{"linux/arm64", "python3", "2.7", "python:2.7", "python3", true},
	} {
		image, program, official, err := pickOwner(owners, tc.platform, tc.name, tc.version)
		if err != nil || image != tc.image || program != tc.program || official != tc.official {
			t.Errorf("%s@%s on %s: %s %s %v %v", tc.name, tc.version, tc.platform, image, program, official, err)
		}
	}
	if _, program, _, _ := pickOwner(owners[2:], "linux/arm64", "jq", ""); program != "/jq" {
		t.Errorf("an entrypoint outside PATH runs by its path, not %q", program)
	}
	if _, _, _, err := pickOwner(owners[:1], "linux/arm64", "nvcc", ""); err == nil {
		t.Error("an owner for another platform was picked")
	}
}

// Away from a terminal, as in a Makefile, only an image allowed before runs
// a command, unless it is official.
func TestApprovalAwayFromATerminal(t *testing.T) {
	t.Setenv("RANGE_CACHE_DIR", t.TempDir())
	if approved("texlive/texlive:latest") {
		t.Fatal("an image never allowed ran without a terminal")
	}
	if err := allow("texlive/texlive"); err != nil {
		t.Fatal(err)
	}
	if !approved("texlive/texlive:2025") {
		t.Error("an allowed repository did not run")
	}
}
