package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"

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
// run, and Range does not. Lima's VM is long-lived, so "no daemon" is only
// claimed where it is true: natively, and in Range's own VM, which lives as
// long as its session.
func NotNeeded(host Runtime) string {
	switch host.Name() {
	case "native":
		return "a daemon, a registry, a container runtime, a snapshotter, FUSE"
	case "kvm":
		return "root, a daemon, a registry, a container runtime, a snapshotter, FUSE"
	case "vz":
		return "a daemon, a registry, a container runtime, a snapshotter, FUSE, Lima"
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
	// Mounts are further remote filesystems shown read-only inside the
	// environment.
	Mounts []Mount
	// Dirs are directories of this machine shown read-write inside it.
	Dirs []Dir
	// Env sets variables in the environment, KEY=VALUE, over the image's.
	Env []string
	// Ports are published on this machine while the session runs.
	Ports []Port
	// DefaultCommand runs the environment's own command when Command is
	// empty, instead of a shell.
	DefaultCommand bool
	// ControllingTerminal starts the workload in a session of its own with
	// standard input as its controlling terminal. Range's VM on a Mac needs
	// it: there, Range is the first process, and no login gave it a terminal.
	ControllingTerminal bool
}

// ExitStatus is a workload's non-zero exit status. range exits with it, so a
// script running "range run" sees the status of its command.
type ExitStatus int

func (e ExitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// exitStatusOf turns the error a workload's process ended with into an
// ExitStatus, if it is one.
func exitStatusOf(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code > 0 {
			return ExitStatus(code)
		}
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return ExitStatus(128 + int(status.Signal()))
		}
	}
	return err
}

// Select returns the runtime for this host.
func Select() Runtime {
	switch runtime.GOOS {
	case "linux":
		// Natively as root: nothing is faster. Without root, in a VM, where
		// KVM and QEMU allow one. RANGE_RUNTIME=native or kvm chooses.
		mode := os.Getenv("RANGE_RUNTIME")
		if mode != "native" && (mode == "kvm" || os.Geteuid() != 0) {
			if kvm, ok := newKVM(); ok {
				return kvm
			}
		}
		return Native{}
	case "darwin":
		// Range's own VM where this Mac and this build can boot one; Lima
		// otherwise, or when RANGE_RUNTIME=lima asks for it.
		if vz, ok := newVZ(); ok {
			return vz
		}
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
