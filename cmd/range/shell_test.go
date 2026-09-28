package main

import (
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/rangetest"
)

// The banner must not emit CRLF when it is being captured rather than shown.
func TestBannerLineUsesPlainNewlineOffTerminal(t *testing.T) {
	out, restore := rangetest.CaptureStdout(t)
	bannerLine("  Ready             %s", "1.23 s")
	restore()
	got := out()
	if strings.Contains(got, "\r") {
		t.Errorf("captured output carries a carriage return: %q", got)
	}
	if got != "  Ready             1.23 s\n" {
		t.Errorf("banner line = %q", got)
	}
}
