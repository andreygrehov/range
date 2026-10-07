package session

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/andreygrehov/range/internal/environment"
	"github.com/andreygrehov/range/internal/tool"
)

// A disk of another architecture, such as an x86 server's EBS snapshot opened
// on an Apple silicon Mac, holds programs this machine cannot run. Range can
// still open it, with tools of its own: a static busybox at toolsDir, first
// in PATH, and its sh as the shell. The root stays the disk's, so cat
// /etc/os-release reads the disk's file.

// toolsDir is where an environment finds Range's own tools.
const toolsDir = "/.range/bin"

// environmentArch names the architecture of the programs at root, from its
// /bin/sh, or "" when it cannot tell.
func environmentArch(root string) string {
	sh, err := resolveIn(root, "/bin/sh")
	if err != nil {
		return ""
	}
	return executableArch(sh)
}

// resolveIn follows path's symbolic links inside root, as a process chrooted
// there would: an absolute link starts again at root, and nothing leads out.
func resolveIn(root, path string) (string, error) {
	rest := strings.Split(strings.Trim(path, "/"), "/")
	resolved := "/"
	for links := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		next := filepath.Join(resolved, name) // Join cleans "..", and never above "/"
		info, err := os.Lstat(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if links++; links > 40 {
			return "", fmt.Errorf("%s: too many links", path)
		}
		target, err := os.Readlink(filepath.Join(root, next))
		if err != nil {
			return "", err
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		rest = append(strings.Split(strings.Trim(target, "/"), "/"), rest...)
	}
	return filepath.Join(root, resolved), nil
}

// provideTools shows busybox at toolsDir inside the session's root, with a
// link for each of its programs. Nothing of it stays in the environment's own
// files but the empty directory it is mounted on.
func provideTools(sess *Session, busybox string) error {
	out, err := exec.Command(busybox, "--list").Output()
	if err != nil {
		return fmt.Errorf("list the programs of %s: %w", busybox, err)
	}
	dir, err := makeDirIn(sess.root, filepath.Dir(toolsDir))
	if err != nil {
		return err
	}
	if err := tool.Run("mount", "-t", "tmpfs", "-o", "mode=755,nosuid,nodev", "range-tools", dir); err != nil {
		return err
	}
	sess.Push(func() error { return tool.Run("umount", "-l", dir) })
	bin := filepath.Join(dir, filepath.Base(toolsDir))
	if err := os.Mkdir(bin, 0o755); err != nil {
		return err
	}
	target := filepath.Join(bin, "busybox")
	if err := os.WriteFile(target, nil, 0o755); err != nil {
		return err
	}
	if err := tool.Run("mount", "--bind", "-o", "ro", busybox, target); err != nil {
		return err
	}
	for _, name := range strings.Fields(string(out)) {
		if name == "busybox" || strings.Contains(name, "/") {
			continue
		}
		if err := os.Symlink("busybox", filepath.Join(bin, name)); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

// toolsIfForeign gives the environment Range's own tools when the programs
// at its root are built for another architecture.
func toolsIfForeign(sess *Session, meta *environment.Metadata, busybox string) error {
	arch := environmentArch(sess.lower)
	if arch == "" || arch == runtime.GOARCH {
		return nil
	}
	if err := provideTools(sess, busybox); err != nil {
		return fmt.Errorf("show Range's own tools: %w", err)
	}
	meta.Shell = toolsDir + "/sh"
	meta.Environment = withToolsFirst(meta.Environment)
	fmt.Fprintf(os.Stderr, "range: the programs here are built for linux/%s, and this machine runs linux/%s.\n"+
		"  The shell and its commands are Range's own, from busybox at %s.\n", arch, runtime.GOARCH, toolsDir)
	return nil
}

// withToolsFirst puts toolsDir first in PATH.
func withToolsFirst(env map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range env {
		out[k] = v
	}
	path := out["PATH"]
	if path == "" {
		path = defaultPath
	}
	out["PATH"] = toolsDir + ":" + path
	return out
}
