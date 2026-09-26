package session

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/image"
)

// Range Core (S3, HTTP, cache, profiles, credentials) always runs on the host.
// Only the Linux-specific half — nbd attach, ext4, overlay, namespaces — needs
// a Linux kernel. On Linux that is this machine; elsewhere it is a small reusable
// Linux VM that the host reaches over the NBD protocol it already speaks.
//
//	host: Range Core + credentials + cache + profiles
//	  |
//	  | NBD
//	  v
//	guest: ext4 -> overlay -> namespaces -> workload
type Runtime interface {
	Name() string
	// run executes a workload inside the environment described by opts.
	Run(ctx context.Context, opts Options, onReady func()) error
	BuildImage(ctx context.Context, req image.BuildRequest) error
	// requirements reports what the host still needs, for range doctor.
	Requirements() []Requirement
}

// Requirement is one thing a runtime needs from the host, whether it is
// present, and how to get it.
type Requirement struct {
	What   string
	OK     bool
	Detail string
	// Fixable marks something range provisions itself on first use, so it must
	// not block the shell the way a missing dependency does.
	Fixable bool
	// From says where the thing comes from. The list is short on purpose:
	// Range adds only what the kernel and a stock distribution do not have.
	From string
}

// NotNeeded is what every lazy-loading alternative asks you to install and
// run, and Range does not. On macOS the Linux VM is itself long-lived, so
// "no daemon" is only claimed where it is true.
func NotNeeded(host Runtime) string {
	if _, native := host.(Native); native {
		return "a daemon, a registry, a container runtime, a snapshotter, FUSE"
	}
	return "a registry, a container runtime, a snapshotter, FUSE\n" +
		"  (the Linux VM is the one long-running piece on this host)"
}

// Options is everything range shell resolved before touching a kernel.
type Options struct {
	Reader    *core.Reader
	Config    core.Config
	Session   *Session
	Workload  string
	Workdir   string
	ShellPath string
	Command   []string
	UpperDir  string
	EnvName   string
	Keep      bool
}

// Select returns the runtime for this host.
func Select() Runtime {
	switch runtime.GOOS {
	case "linux":
		return Native{}
	case "darwin":
		return lima{instance: limaInstance}
	default:
		// Windows runs the Linux build inside WSL2 rather than as a native
		// binary: the namespace and nbd code has no Windows equivalent.
		return unsupported{}
	}
}

type unsupported struct{}

func (unsupported) Name() string { return runtime.GOOS }

func (unsupported) Run(context.Context, Options, func()) error {
	return errors.New(unsupportedHint())
}

// unsupportedHint explains the supported route for this host.
func unsupportedHint() string {
	if runtime.GOOS == "windows" {
		return "range shell is not a native Windows binary; run it inside WSL2:" +
			"\n    wsl --install -d Ubuntu" +
			"\n    wsl sudo modprobe nbd nbds_max=16" +
			"\n    wsl range shell s3://bucket/dev.img"
	}
	return "range has no environment runtime for " + runtime.GOOS
}

func (unsupported) BuildImage(context.Context, image.BuildRequest) error {
	return errors.New(unsupportedHint())
}

func (unsupported) Requirements() []Requirement {
	return []Requirement{{What: "Linux runtime", OK: false, Detail: unsupportedHint()}}
}

// CheckRequirements fails with one message naming everything the host is
// missing.
func CheckRequirements(host Runtime) error {
	var missing []string
	for _, req := range host.Requirements() {
		if !req.OK && !req.Fixable {
			missing = append(missing, fmt.Sprintf("  %-14s %s", req.What, req.Detail))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("this host is not ready to run an environment (%s runtime):\n%s\n\nRun \"range doctor\" for the full picture",
		host.Name(), strings.Join(missing, "\n"))
}
