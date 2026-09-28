package main

import (
	"testing"

	"github.com/andreygrehov/range/internal/rangetest"
)

func TestImageBuildRejectsMissingArguments(t *testing.T) {
	rangetest.QuietStdout(t)
	if err := commandImage(nil); err == nil {
		t.Fatal("expected an error with no subcommand")
	}
	if err := commandImage([]string{"build"}); err == nil {
		t.Fatal("expected an error with no rootfs")
	}
	if err := commandImage([]string{"build", t.TempDir()}); err == nil {
		t.Fatal("expected an error with no --size")
	}
}

func TestWantsPacking(t *testing.T) {
	for _, tc := range []struct {
		format, output string
		want           bool
	}{
		// The compressed artifact is the default; raw needs asking for.
		{"", "dev.img", true}, {"range", "dev.img", true},
		{"raw", "dev.range", false},
		{"auto", "dev.range", true}, {"auto", "dev.img", false},
	} {
		got, err := wantsPacking(tc.format, tc.output)
		if err != nil || got != tc.want {
			t.Errorf("wantsPacking(%q,%q) = %v,%v want %v", tc.format, tc.output, got, err, tc.want)
		}
	}
	if _, err := wantsPacking("brotli", "x"); err == nil {
		t.Error("an unknown format was accepted")
	}
}

func TestImageBuildRejectsMissingSource(t *testing.T) {
	rangetest.QuietStdout(t)
	if err := commandImage([]string{"build", "--size", "1GiB"}); err == nil {
		t.Fatal("expected an error with neither a rootfs nor --from-oci")
	}
}
