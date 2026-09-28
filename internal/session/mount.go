package session

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/andreygrehov/range/internal/environment"
	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/tool"
)

// overlayOptions builds the mount options for the merged root.
func overlayOptions(lower, upper, work string) string {
	return fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
}

// DetectFilesystem names the filesystem on a device from its superblock magic,
// so mount is told what to expect instead of probing, and an artifact holding
// neither gets an error that says so.
func DetectFilesystem(device string) (string, error) {
	f, err := os.Open(device)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return filesystemOf(f)
}

func filesystemOf(r io.ReaderAt) (string, error) {
	super := make([]byte, 64)
	if _, err := r.ReadAt(super, erofs.SuperOffset); err != nil {
		return "", fmt.Errorf("read the superblock: %w", err)
	}
	switch {
	case binary.LittleEndian.Uint32(super[0:]) == erofs.Magic:
		return "erofs", nil
	case binary.LittleEndian.Uint16(super[56:]) == 0xef53:
		return "ext4", nil
	}
	return "", errors.New("the artifact holds neither EROFS nor ext4; build one with range build")
}

// MountAndRun is shared by every runtime: it takes an attached block device and
// produces a workload running inside the environment.
func MountAndRun(ctx context.Context, opts Options, device string, onReady func()) error {
	sess := opts.Session
	sess.State = StateMounting
	fsType, err := DetectFilesystem(device)
	if err != nil {
		return err
	}
	if err := tool.Run("mount", "-t", fsType, "-o", "ro", device, sess.lower); err != nil {
		return fmt.Errorf("mount the %s filesystem in the artifact: %w", fsType, err)
	}
	sess.Push(func() error { return tool.Run("umount", sess.lower) })
	if err := tool.Run("mount", "-t", "overlay", "overlay", "-o",
		overlayOptions(sess.lower, sess.Upper, sess.work), sess.root); err != nil {
		return fmt.Errorf("stack the writable overlay: %w", err)
	}
	sess.Push(func() error { return tool.Run("umount", sess.root) })
	if err := mountExtras(sess, opts.Mounts); err != nil {
		return err
	}

	meta := environment.ReadMetadata(sess.lower)
	if host := "linux/" + runtime.GOARCH; meta.Platform != "" && meta.Platform != host {
		return fmt.Errorf("this artifact was built for %s but the environment runs %s; "+
			"rebuild it with range build --platform %s, on any host", meta.Platform, host, host)
	}
	if opts.Workdir != "" {
		meta.Workdir = opts.Workdir
	}
	if opts.ShellPath != "" {
		meta.Shell = opts.ShellPath
	}
	command := opts.Command
	if len(command) == 0 && opts.DefaultCommand {
		if command = meta.Command(); len(command) == 0 {
			return errors.New("this environment names no command of its own; give one after --")
		}
	}
	sess.Name = meta.Name
	if sess.Name == "" {
		sess.Name = opts.EnvName
	}
	sess.Meta = meta

	cfg := sessionConfig{
		Root: sess.root, Shell: meta.Shell, Workdir: meta.Workdir,
		Hostname: meta.Hostname, Environment: meta.Environment, Command: command,
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sess.dir, "session.json"), encoded, 0o644); err != nil {
		return err
	}
	sess.State = StateRunning
	return runInNamespaces(ctx, sess.dir, onReady)
}
