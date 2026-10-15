package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/egress"
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
	// LoadFilesystem loads the kernel module of a filesystem the kernel
	// cannot load by itself, as in Range's VM, which has no modprobe.
	LoadFilesystem func(fsType string) error
	// Commands runs a command the environment lacks from the image that
	// holds it, or nil.
	Commands Finder
	// OpenImage attaches an image as a block device, for Commands. Nil
	// opens it here and attaches it natively; Range's VM asks its host.
	OpenImage func(ctx context.Context, image string) (device string, detach func() error, err error)
	// User runs the workload as another user than root, or nil.
	User *User
	// CACerts are authorities this machine trusts, added to the
	// environment's, as HostCACerts gives them.
	CACerts []byte
	// Tools is a static busybox, for an environment whose own programs are
	// built for another architecture, or "".
	Tools string
	// NVIDIA is the host's NVIDIA driver, shown inside the environment, or
	// nil. Only the native runtime takes it.
	NVIDIA *NVIDIA
	// Limits bound what the session takes of this machine.
	Limits Limits
	// Network is NetworkNone for a session without a network of its own;
	// Egress then lets it reach the hosts it allows, through a proxy on
	// this machine, and EgressLog is told of each request.
	Network   string
	Egress    *egress.Policy
	EgressLog func(host string, port int, allowed bool)
	// Idle runs no workload: the session waits for range exec, until range
	// stop ends it.
	Idle bool
	// Control serves the session's socket for range exec, range cp and
	// range stop. Executor runs those requests in the guest of Range's VM.
	Control  bool
	Executor *Executor
	// DefaultCommand runs the environment's own command when Command is
	// empty, instead of a shell.
	DefaultCommand bool
	// ControllingTerminal starts the workload in a session of its own with
	// standard input as its controlling terminal. Range's VM on a Mac needs
	// it: there, Range is the first process, and no login gave it a terminal.
	ControllingTerminal bool
}

// User is the user and group a workload runs as, by number.
type User struct {
	UID int `json:"uid"`
	GID int `json:"gid"`
}

// ParseUser reads UID[:GID]. Without a GID, the group is the UID's number.
func ParseUser(s string) (*User, error) {
	uid, gid, hasGID := strings.Cut(s, ":")
	u := &User{}
	var err error
	if u.UID, err = strconv.Atoi(uid); err != nil || u.UID < 0 {
		return nil, fmt.Errorf("--user %q: want UID[:GID], by number", s)
	}
	u.GID = u.UID
	if hasGID {
		if u.GID, err = strconv.Atoi(gid); err != nil || u.GID < 0 {
			return nil, fmt.Errorf("--user %q: want UID[:GID], by number", s)
		}
	}
	return u, nil
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

// Select returns the runtime a session gets by default on this host.
func Select() Runtime {
	host, _, err := Choose("", "")
	if err != nil {
		return Native{}
	}
	return host
}

// Choose returns the runtime for a session. name is --runtime, or else
// RANGE_RUNTIME: native, kvm or vm on Linux, vz, vm or lima on a Mac. "" is
// the default: a VM of the session's own, the security boundary the native
// runtime is not. nativeOnly names what the session asks for that only the
// native runtime does, such as --gpus: it picks the native runtime when none
// is named, and refuses a VM. note is a line to show when the default could
// not be a VM.
func Choose(name, nativeOnly string) (host Runtime, note string, err error) {
	if name == "" {
		name = os.Getenv("RANGE_RUNTIME")
	}
	switch runtime.GOOS {
	case "linux":
		switch name {
		case "native":
			return Native{}, "", nil
		case "kvm", "vm":
			if nativeOnly != "" {
				return nil, "", fmt.Errorf("%s needs the native runtime, not a VM: --runtime native", nativeOnly)
			}
			kvm, ok := newKVM()
			if !ok {
				return nil, "", kvmUnavailable()
			}
			return kvm, "", nil
		case "":
			if nativeOnly != "" {
				return Native{}, "", nil
			}
			if kvm, ok := newKVM(); ok {
				return kvm, "", nil
			}
			if os.Geteuid() == 0 {
				note = "range: this machine cannot run Range's VM (" + kvmMissing() + "), so the session runs " +
					"natively, which is not a security boundary. --runtime native says so; --runtime kvm requires the VM"
			}
			return Native{}, note, nil
		}
		return nil, "", fmt.Errorf("--runtime %q: on Linux, native, kvm or vm", name)
	case "darwin":
		switch name {
		case "", "vz", "vm":
			if nativeOnly != "" {
				return nil, "", fmt.Errorf("%s needs Linux and the native runtime", nativeOnly)
			}
			// Range's own VM where this Mac and this build can boot one;
			// Lima otherwise.
			if vz, ok := newVZ(); ok {
				return vz, "", nil
			}
			if name != "" {
				return nil, "", errors.New("--runtime vz needs an Apple silicon Mac with macOS 14 or later")
			}
			return lima{instance: limaInstance}, "", nil
		case "lima":
			return lima{instance: limaInstance}, "", nil
		}
		return nil, "", fmt.Errorf("--runtime %q: on a Mac, vz, vm or lima", name)
	default:
		// Windows runs the Linux build inside WSL2 rather than as a native
		// binary: the namespace and nbd code has no Windows equivalent.
		return unsupported{}, "", nil
	}
}

// missing names what a runtime lacks on this host.
func missing(host Runtime) string {
	var names []string
	for _, req := range host.Requirements() {
		if !req.OK && !req.Fixable {
			names = append(names, "no "+req.What)
		}
	}
	return strings.Join(names, ", ")
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
