package oci

import (
	"testing"
)

func TestPlatform(t *testing.T) {
	for _, bad := range []string{"amd64", "darwin/arm64", "linux/", "linux/arm64/v8/x"} {
		if _, err := ParsePlatform(bad); err == nil {
			t.Errorf("parsePlatform(%q) was accepted", bad)
		}
	}
	amd, err := ParsePlatform("linux/amd64")
	if err != nil || amd.String() != "linux/amd64" {
		t.Fatalf("linux/amd64 = %v, %v", amd, err)
	}
	arm, _ := ParsePlatform("linux/arm64")
	if !arm.matches("linux", "arm64", "v8") || arm.matches("linux", "amd64", "") {
		t.Error("linux/arm64 must match any arm64 variant and nothing else")
	}
	v7, _ := ParsePlatform("linux/arm/v7")
	if v7.matches("linux", "arm", "v6") || !v7.matches("linux", "arm", "v7") {
		t.Error("an explicit variant must be matched exactly")
	}
	meta := environmentFor("x", ImageConfig{}, amd, func(string) bool { return false })
	if meta.Platform != "linux/amd64" {
		t.Errorf("environment.json platform = %q, want linux/amd64", meta.Platform)
	}
}
