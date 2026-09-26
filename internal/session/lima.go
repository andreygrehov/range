package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/andreygrehov/range/internal/image"
	"github.com/andreygrehov/range/internal/nbd"
	"github.com/andreygrehov/range/internal/tool"
)

// limaInstance is the VM Range uses on macOS. It is small and disposable: the
// environment itself lives in object storage, not in the VM.
var limaInstance = envOr("RANGE_LIMA_INSTANCE", "range-linux")

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

// guestBinary is where the Linux build of range lives inside the VM.
const guestBinary = "/usr/local/bin/range"

type lima struct{ instance string }

func (lima) Name() string { return "lima" }

func (l lima) vmStatus() string {
	out, err := exec.Command("limactl", "list", "--format", "{{.Name}}\t{{.Status}}", l.instance).Output()
	if err != nil {
		return "absent"
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "absent"
	}
	return strings.ToLower(fields[1])
}

func (l lima) Requirements() []Requirement {
	reqs := []Requirement{}
	_, err := exec.LookPath("limactl")
	reqs = append(reqs, Requirement{"limactl", err == nil,
		"install Lima:  brew install lima", false, "brew install lima"})
	if err != nil {
		return reqs
	}
	// Everything below this point range provisions itself on first use, so a
	// missing VM or guest binary is reported as informational rather than fatal.
	status := l.vmStatus()
	reqs = append(reqs, Requirement{"Linux VM", status == "running",
		fmt.Sprintf("VM %q is %s; range will create and start it on first use", l.instance, status), true,
		"created by range on first use; nothing installed inside it"})
	if _, err := guestBinarySource(); err != nil {
		reqs = append(reqs, Requirement{"guest binary", false, err.Error(), false, "range"})
	}
	return reqs
}

// guestBinarySource locates a Linux build of range to install in the VM. In
// order of preference: an explicit path, a sibling of this executable, or a
// cross-compile from the source tree when a Go toolchain is available.
func guestBinarySource() (string, error) {
	if path := os.Getenv("RANGE_GUEST_BINARY"); path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("RANGE_GUEST_BINARY=%s does not exist", path)
		}
		return path, nil
	}
	// A source tree wins over any prebuilt copy. Cross-compiling is incremental
	// and takes well under a second, and the alternative is shipping a guest
	// that is quietly older than the host - which shows up as the VM rejecting
	// commands the host has, rather than as anything resembling a version
	// problem.
	if dir := moduleDir(); dir != "" {
		if _, err := exec.LookPath("go"); err == nil {
			out, err := guestBuildPath()
			if err != nil {
				return "", err
			}
			build := exec.Command("go", "build", "-o", out, "./cmd/range")
			build.Dir = dir
			build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH)
			if output, err := build.CombinedOutput(); err != nil {
				return "", fmt.Errorf("cross-compiling the guest binary failed: %s",
					strings.TrimSpace(string(output)))
			}
			return out, nil
		}
	}
	// A released binary may ship the Linux build beside it. This is checked
	// after the source tree so a stale copy can never shadow current code.
	if self, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(self), "range-linux-"+runtime.GOARCH)
		if _, err := os.Stat(sibling); err == nil {
			return sibling, nil
		}
	}
	return "", fmt.Errorf("no Linux build of range available; set RANGE_GUEST_BINARY, "+
		"or place range-linux-%s next to this binary, "+
		"or run from the source tree with Go installed", runtime.GOARCH)
}

// guestBuildPath is where the cross-compiled guest binary is kept. It is
// deliberately not os.TempDir(): a binary executed from there would then find
// its own build output sitting next to it and treat it as a shipped sibling.
func guestBuildPath() (string, error) {
	home, err := os.UserCacheDir()
	if err != nil {
		home = os.TempDir()
	}
	dir := filepath.Join(home, "range", "guest")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "range-linux-"+runtime.GOARCH), nil
}

// moduleDir finds the source tree, looking beside the executable and then at
// the working directory.
func moduleDir() string {
	candidates := []string{}
	if self, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(self), filepath.Dir(filepath.Dir(self)))
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates, cwd)
	}
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "cmd", "range", "main.go")); err == nil {
				return dir
			}
		}
	}
	return ""
}

// ensure brings the VM up to the state range shell needs. It is idempotent and
// costs two quick checks once everything is in place.
func (l lima) ensure(ctx context.Context) error {
	switch l.vmStatus() {
	case "running":
	case "absent":
		fmt.Fprintf(os.Stderr, "range: creating Linux VM %q (first run only, a few minutes)\n", l.instance)
		create := exec.CommandContext(ctx, "limactl", "start", "--name="+l.instance,
			"--vm-type=vz", "--cpus=4", "--memory=8", "--disk=40", "--tty=false",
			"template://ubuntu-24.04")
		create.Stdout, create.Stderr = os.Stderr, os.Stderr
		if err := create.Run(); err != nil {
			return fmt.Errorf("could not create the Linux VM: %w", err)
		}
	default:
		fmt.Fprintf(os.Stderr, "range: starting Linux VM %q\n", l.instance)
		start := exec.CommandContext(ctx, "limactl", "start", l.instance, "--tty=false")
		start.Stdout, start.Stderr = os.Stderr, os.Stderr
		if err := start.Run(); err != nil {
			return fmt.Errorf("could not start the Linux VM: %w", err)
		}
	}

	// The nbd module is not persistent across boots, so load it every time.
	if err := l.guestExec("sudo", "modprobe", "nbd", "nbds_max=16"); err != nil {
		return fmt.Errorf("could not load the nbd module in the VM: %w", err)
	}
	// Nothing is installed in the VM beyond range itself: the NBD client is
	// built in, EROFS is in the kernel, and mount/unshare ship with the image.
	if err := l.ensureGuestBinary(ctx); err != nil {
		return err
	}
	return nil
}

// ensureGuestBinary installs the Linux build in the VM when it is missing or
// older than the one on this host.
func (l lima) ensureGuestBinary(ctx context.Context) error {
	source, err := guestBinarySource()
	if err != nil {
		return err
	}
	sum, err := fileChecksum(source)
	if err != nil {
		return err
	}
	if l.guestExec("sh", "-c", "test -f "+guestBinary+".sha && grep -q "+sum+" "+guestBinary+".sha") == nil {
		return nil
	}
	fmt.Fprintln(os.Stderr, "range: installing the Linux build of range in the VM")
	copyIn := exec.CommandContext(ctx, "limactl", "copy", source, l.instance+":/tmp/range-guest")
	copyIn.Stderr = os.Stderr
	if err := copyIn.Run(); err != nil {
		return fmt.Errorf("could not copy the guest binary into the VM: %w", err)
	}
	install := exec.CommandContext(ctx, "limactl", "shell", "--workdir", "/", l.instance,
		"sudo", "sh", "-c", "install /tmp/range-guest "+guestBinary+" && echo "+sum+" > "+guestBinary+".sha")
	install.Stderr = os.Stderr
	if err := install.Run(); err != nil {
		return fmt.Errorf("could not install the guest binary: %w", err)
	}
	return nil
}

func fileChecksum(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:16], nil
}

func (l lima) guestExec(args ...string) error {
	cmd := exec.Command("limactl", append([]string{"shell", "--workdir", "/", l.instance}, args...)...)
	return cmd.Run()
}

// Run keeps Range Core, credentials, the cache and the profiles on the Mac and
// exposes the artifact to the VM over NBD through an SSH reverse tunnel, so the
// guest needs no AWS access and there is only ever one cache.
func (l lima) Run(ctx context.Context, opts Options, onReady func()) error {
	if err := l.ensure(ctx); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port

	served := make(chan struct{})
	go func() {
		defer close(served)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if err := nbd.ServerHandshake(conn, opts.Reader.Size()); err != nil {
					log.Printf("range: nbd handshake: %v", err)
					return
				}
				if err := nbd.Serve(conn, opts.Reader, opts.Reader.Size()); err != nil {
					log.Printf("range: nbd session: %v", err)
				}
			}()
		}
	}()
	opts.Session.Push(func() error { listener.Close(); <-served; return nil })

	sshConfig := filepath.Join(os.Getenv("HOME"), ".lima", l.instance, "ssh.config")
	if _, err := os.Stat(sshConfig); err != nil {
		return fmt.Errorf("lima ssh config not found at %s: %w", sshConfig, err)
	}
	// A second reverse-forwarded port carries the guest's readiness signal back,
	// so the reported time to shell means the same thing on macOS as on Linux:
	// mounted, namespaced, chrooted, about to exec.
	readyListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer readyListener.Close()
	readyPort := readyListener.Addr().(*net.TCPAddr).Port
	go func() {
		conn, err := readyListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
			onReady()
			conn.Write([]byte{'G'}) // release the guest once the banner is printed
		}
	}()

	guest := []string{
		"sudo", guestBinary, GuestCommand,
		"--nbd", fmt.Sprintf("127.0.0.1:%d", port),
		"--ready-port", strconv.Itoa(readyPort),
		"--session", opts.Session.ID,
		"--workload", opts.Workload,
	}
	if opts.Workdir != "" {
		guest = append(guest, "--workdir", opts.Workdir)
	}
	if opts.ShellPath != "" {
		guest = append(guest, "--shell", opts.ShellPath)
	}
	if len(opts.Command) > 0 {
		guest = append(guest, "--")
		guest = append(guest, opts.Command...)
	}
	// ssh joins its arguments and the remote shell splits them again, so each
	// one has to be quoted or a workload command falls apart on the way over.
	quoted := make([]string, len(guest))
	for i, arg := range guest {
		quoted[i] = shellQuote(arg)
	}
	// -R makes the guest's localhost:port reach the NBD server on this Mac.
	args := []string{"-F", sshConfig,
		"-R", fmt.Sprintf("%d:127.0.0.1:%d", port, port),
		"-R", fmt.Sprintf("%d:127.0.0.1:%d", readyPort, readyPort)}
	if isTerminal(os.Stdin) {
		args = append(args, "-t") // interactive shell wants a pty
	} else {
		args = append(args, "-T")
	}
	args = append(args, limaSSHHost(l.instance), strings.Join(quoted, " "))
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func limaSSHHost(instance string) string { return "lima-" + instance }

// shellQuote makes one argument survive a trip through a remote shell.
func shellQuote(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// BuildImage runs the build inside the VM and lands the result on the host
// through lima's shared /tmp/lima mount, so the user sees one command and one
// output file wherever they asked for it.
func (l lima) BuildImage(ctx context.Context, req image.BuildRequest) error {
	if err := l.ensure(ctx); err != nil {
		return err
	}
	output, err := filepath.Abs(req.Output)
	if err != nil {
		return err
	}
	shared := filepath.Join("/tmp/lima", "range-build-"+NewID())
	if err := os.MkdirAll(shared, 0o755); err != nil {
		return fmt.Errorf("create the shared build directory: %w", err)
	}
	defer os.RemoveAll(shared)
	staged := filepath.Join(shared, filepath.Base(output))

	guestArgs := []string{"shell", "--workdir", "/", l.instance,
		"sudo", guestBinary, "build", "--format", "raw", "--fs", req.FS,
		"--chunk-size", strconv.FormatInt(req.Align, 10),
		"--size", strconv.FormatInt(req.Size, 10), "--output", staged}
	if req.FromOCI != "" {
		guestArgs = append(guestArgs, "--from-oci", req.FromOCI, "--platform", req.Platform.String())
	} else {
		// A local rootfs has to reach the guest; hand it over through the mount.
		hostRootfs := filepath.Join(shared, "rootfs")
		if err := tool.Run("cp", "-a", req.Rootfs, hostRootfs); err != nil {
			return err
		}
		guestArgs = append(guestArgs, hostRootfs)
	}
	build := exec.CommandContext(ctx, "limactl", guestArgs...)
	// The guest's success message names a temporary file. Only the host can
	// report the final artifact; keep the guest's progress and errors visible.
	build.Stdout, build.Stderr = io.Discard, os.Stderr
	if err := build.Run(); err != nil {
		return fmt.Errorf("building the image inside the VM failed: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	os.Remove(output)
	if err := os.Rename(staged, output); err != nil {
		// Rename fails across filesystems; fall back to a sparse-preserving copy.
		if copyErr := tool.Run("cp", "--sparse=always", staged, output); copyErr != nil {
			return fmt.Errorf("move the built image to %s: %w", output, err)
		}
	}
	return nil
}

// GuestCommand is the hidden subcommand the host runs inside the Lima VM.
const GuestCommand = "__guest"
