package oci

import (
	"fmt"
	"runtime"
	"strings"
)

// Platform is the platform an artifact is built for. It defaults to the
// machine running the build, but a build can run anywhere now, so a Mac can
// build for the x86 fleet it publishes to.
type Platform struct{ os, arch, variant string }

// HostPlatform is linux on the architecture range is running on.
func HostPlatform() Platform { return Platform{os: "linux", arch: runtime.GOARCH} }

// String formats the platform as os/arch[/variant].
func (p Platform) String() string {
	if p.variant != "" {
		return p.os + "/" + p.arch + "/" + p.variant
	}
	return p.os + "/" + p.arch
}

// matches accepts any variant when none was asked for, so linux/arm64 picks
// the linux/arm64/v8 image registries usually label it with.
func (p Platform) matches(os, arch, variant string) bool {
	return os == p.os && arch == p.arch && (p.variant == "" || variant == p.variant)
}

// ParsePlatform reads linux/ARCH or linux/ARCH/VARIANT.
func ParsePlatform(value string) (Platform, error) {
	parts := strings.Split(value, "/")
	if len(parts) < 2 || len(parts) > 3 || parts[0] != "linux" || parts[1] == "" {
		return Platform{}, fmt.Errorf("--platform %q: want linux/ARCH, e.g. linux/amd64 or linux/arm64", value)
	}
	p := Platform{os: parts[0], arch: parts[1]}
	if len(parts) == 3 {
		p.variant = parts[2]
	}
	return p, nil
}
