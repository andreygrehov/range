package main

import (
	"errors"
	"slices"
	"testing"

	"github.com/andreygrehov/range/internal/commands"
)

func TestPickOwner(t *testing.T) {
	owners := []commands.Owner{
		{Repo: "kasmweb/desktop", Platforms: []string{"linux/arm64"}, Tag: "1"},
		{Repo: "nvidia/cuda", Platforms: []string{"linux/amd64", "linux/arm64"}, Tag: "12.8.0-devel-ubuntu24.04", Trusted: true},
		{Repo: "ghcr.io/jqlang/jq", Platforms: []string{"linux/arm64"}, Tag: "latest", Path: "/jq", Trusted: true},
	}
	arm := func(o commands.Owner) bool { return o.Trusted && slices.Contains(o.Platforms, "linux/arm64") }
	// The first trusted owner built for the platform, with the tag the catalog read.
	if image, program, err := pickOwner(owners, arm, "nvcc", ""); err != nil || image != "nvidia/cuda:12.8.0-devel-ubuntu24.04" || program != "nvcc" {
		t.Errorf("nvcc: %s %s %v", image, program, err)
	}
	// A version is a tag.
	if image, _, _ := pickOwner(owners[1:2], arm, "nvcc", "12.6.3-devel-ubuntu24.04"); image != "nvidia/cuda:12.6.3-devel-ubuntu24.04" {
		t.Errorf("a version: %s", image)
	}
	// A program outside PATH runs by its path.
	if _, program, _ := pickOwner(owners[2:], arm, "jq", ""); program != "/jq" {
		t.Errorf("jq runs as %q", program)
	}
	// No trusted owner: a clear error, not "not found".
	if _, _, err := pickOwner(owners[:1], arm, "pdflatex", ""); err == nil || errors.Is(err, commands.ErrUnknown) {
		t.Errorf("an untrusted owner only: %v", err)
	}
	if _, _, err := pickOwner(nil, arm, "gti", ""); !errors.Is(err, commands.ErrUnknown) {
		t.Errorf("no owner: %v", err)
	}
}

func TestWithVariant(t *testing.T) {
	for _, c := range []struct{ version, tag, want string }{
		{"3.11", "3.12-slim", "3.11-slim"}, // the catalog's variant
		{"1.22", "1.23", "1.22"},           // no variant
		{"12.6.3", "12.8.0-devel-ubuntu24.04", "12.6.3-devel-ubuntu24.04"},
		{"3.11-bookworm", "3.12-slim", "3.11-bookworm"}, // a variant of its own
		{"2024", "latest", "2024"},                      // a tag that is no version
		{"", "3.12-slim", ""},
	} {
		if got := withVariant(c.version, c.tag); got != c.want {
			t.Errorf("%s in %s: %s, want %s", c.version, c.tag, got, c.want)
		}
	}
}
