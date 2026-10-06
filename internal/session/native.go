package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/image"
	"github.com/andreygrehov/range/internal/nbd"
	"github.com/andreygrehov/range/internal/tool"
)

// Native runs environments on this Linux kernel.
type Native struct{}

// Name identifies the runtime in banners and doctor output.
func (Native) Name() string { return "native" }

// BuildImage builds on this host.
func (Native) BuildImage(_ context.Context, req image.BuildRequest) error {
	return image.BuildNative(req)
}

// Requirements lists what this host needs: root, the nbd, erofs and overlay
// modules, and the util-linux tools.
func (Native) Requirements() []Requirement {
	reqs := []Requirement{{What: "root", OK: os.Geteuid() == 0, Detail: "re-run with sudo", From: "you"}}
	tools := true
	for _, tool := range []string{"mount", "umount", "unshare"} {
		if _, err := exec.LookPath(tool); err != nil {
			tools = false
		}
	}
	reqs = append(reqs, Requirement{What: "mount, unshare", OK: tools,
		Detail: "install util-linux", From: "util-linux, in every distribution"})
	reqs = append(reqs, nbdRequirement(nbdLoaded(), nbdLoadable()))
	reqs = append(reqs, Requirement{What: "erofs", OK: tool.KernelHasFilesystem("erofs"),
		Detail: "needs Linux 5.4 or later built with EROFS; build with --fs ext4 instead",
		From:   "the kernel, 5.4 and later"})
	reqs = append(reqs, Requirement{What: "overlay", OK: tool.KernelHasFilesystem("overlay"),
		Detail: "needs a kernel built with OverlayFS", From: "the kernel"})
	return reqs
}

// nbdRequirement says what nbd still needs. A module that can be loaded is
// range's to load, as root; without root, the root requirement says so.
func nbdRequirement(loaded, loadable bool) Requirement {
	switch {
	case loaded:
		return Requirement{What: "nbd", OK: true, From: "the kernel"}
	case loadable:
		return Requirement{What: "nbd", Fixable: true,
			Detail: "not loaded; range loads it", From: "the kernel; range loads it"}
	}
	return Requirement{What: "nbd", Detail: "this kernel has no nbd module", From: "the kernel"}
}

// Run attaches the artifact to a free nbd device, mounts it and runs the
// workload. Every extra mount gets a device of its own.
func (Native) Run(ctx context.Context, opts Options, onReady func()) error {
	sess, r := opts.Session, opts.Reader
	if err := Prepare(sess, opts.UpperDir, opts.EnvName, opts.Keep); err != nil {
		return err
	}
	sess.State = StateAttaching
	if err := LoadNBD(); err != nil {
		return err
	}
	device, err := attach(sess, r)
	if err != nil {
		return err
	}
	sess.device = device
	for i := range opts.Mounts {
		if opts.Mounts[i].Device, err = attach(sess, opts.Mounts[i].Reader); err != nil {
			return err
		}
	}
	if err := publishOnHost(sess, opts.Ports); err != nil {
		return err
	}
	return MountAndRun(ctx, opts, device, onReady)
}

// attach serves r on a free nbd device and waits until the kernel has it. The
// detach is pushed onto the session's cleanups.
func attach(sess *Session, r *core.Reader) (string, error) {
	device, err := AllocateNBDDevice("/sys/block", "/dev")
	if err != nil {
		return "", err
	}
	attachCtx, stopAttach := context.WithCancel(context.Background())
	var attachErr error
	attachFinished := make(chan struct{})
	go func() {
		attachErr = nbd.Attach(attachCtx, device, r, r.Size())
		close(attachFinished)
	}()
	sess.Push(func() error {
		stopAttach()
		select {
		case <-attachFinished:
		case <-time.After(10 * time.Second):
			return fmt.Errorf("%s did not disconnect", device)
		}
		// The kernel drops the pid file a moment after the disconnect returns.
		// Waiting for it keeps a back-to-back session from grabbing a device
		// that is still being torn down.
		if err := waitForDeviceRelease("/sys/block", device, 10*time.Second); err != nil {
			return err
		}
		return attachErr
	})
	ready := make(chan error, 1)
	go func() { ready <- WaitForBlockDevice("/sys/block", device, 15*time.Second) }()
	select {
	case <-attachFinished:
		return "", fmt.Errorf("attach %s: %w", device, attachErr)
	case err := <-ready:
		if err != nil {
			return "", err
		}
	}
	return device, nil
}
