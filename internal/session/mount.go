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
	"strings"

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
	head := make([]byte, erofs.SuperOffset+64)
	if _, err := r.ReadAt(head, 0); err != nil {
		return "", fmt.Errorf("read the superblock: %w", err)
	}
	super := head[erofs.SuperOffset:]
	switch {
	case binary.LittleEndian.Uint32(super[0:]) == erofs.Magic:
		return "erofs", nil
	case binary.LittleEndian.Uint16(super[56:]) == 0xef53:
		return "ext4", nil
	case string(head[:4]) == "XFSB":
		return "xfs", nil
	}
	return "", errors.New("the artifact holds none of EROFS, ext4 or XFS; build one with range build")
}

// readOnly is the mount options for a read-only filesystem of fsType. A disk
// from a running machine, such as an EBS snapshot, can name journal entries
// to replay, and a read-only disk cannot take them: noload for ext4, and
// norecovery for XFS, mount it as the disk holds it, without the last writes
// still in the journal. nouuid lets XFS mount a copy of a disk this machine
// has mounted already, such as a snapshot of its own root.
func readOnly(fsType string) string {
	switch fsType {
	case "ext4":
		return "ro,noload"
	case "xfs":
		return "ro,norecovery,nouuid"
	}
	return "ro"
}

// loadFilesystem makes sure the kernel has fsType before a mount needs it,
// with the loader the runtime gives, if any.
func loadFilesystem(load func(string) error, fsType string) error {
	if load == nil || tool.KernelRegistered(fsType) {
		return nil
	}
	if err := load(fsType); err != nil {
		return fmt.Errorf("load the %s filesystem: %w", fsType, err)
	}
	return nil
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
	if err := loadFilesystem(opts.LoadFilesystem, fsType); err != nil {
		return err
	}
	if err := tool.Run("mount", "-t", fsType, "-o", readOnly(fsType), device, sess.lower); err != nil {
		return fmt.Errorf("mount the %s filesystem in the artifact: %w", fsType, err)
	}
	sess.Push(func() error { return tool.Run("umount", sess.lower) })
	if err := tool.Run("mount", "-t", "overlay", "overlay", "-o",
		overlayOptions(sess.lower, sess.Upper, sess.work), sess.root); err != nil {
		return fmt.Errorf("stack the writable overlay: %w", err)
	}
	sess.Push(func() error { return tool.Run("umount", sess.root) })
	if err := mountExtras(sess, opts.Mounts, opts.LoadFilesystem); err != nil {
		return err
	}
	if err := bindDirs(sess, opts.Dirs); err != nil {
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
	if len(opts.Env) > 0 && meta.Environment == nil {
		meta.Environment = map[string]string{}
	}
	for _, kv := range opts.Env {
		key, value, _ := strings.Cut(kv, "=")
		meta.Environment[key] = value
	}
	// A disk Range did not build says nothing of its platform: its own
	// programs do.
	if meta.Platform == "" && opts.Tools != "" {
		if err := toolsIfForeign(sess, &meta, opts.Tools); err != nil {
			return err
		}
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
		NVIDIA: opts.NVIDIA,
	}
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sess.dir, "session.json"), encoded, 0o644); err != nil {
		return err
	}
	sess.State = StateRunning
	return runInNamespaces(ctx, sess.dir, opts.ControllingTerminal, onReady)
}
