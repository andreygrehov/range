package session

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"

	"github.com/andreygrehov/range/internal/environment"
)

// insideEnv are the variables a command keeps from the shell that runs it.
// The rest come from its own image.
var insideEnv = []string{"HOME", "TERM", "LANG", "LC_ALL", "TZ", "USER",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy"}

// RunInside runs a command, as a shim found it in the session, in its image's
// root: in a mount namespace of its own, with the session's devices, /proc,
// /sys, /tmp, home, current directory and resolver at their own paths. It
// replaces this process, so the command is the shell's child, as any other.
func RunInside(reply InsideReply, name string, args []string) error {
	// A mount namespace is a thread's own: unshare, the mounts, chroot and
	// exec must all run on one thread, or exec runs in the session's root.
	runtime.LockOSThread()
	meta := environment.ReadMetadata(reply.Lower)
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		return fmt.Errorf("a mount namespace for %s: %w", name, err)
	}
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("a mount namespace for %s: %w", name, err)
	}
	shown := []string{"/dev", "/proc", "/sys", "/tmp", os.Getenv("HOME"), "/usr/local/nvidia", cwd}
	for _, dir := range shown {
		if dir == "" || dir == "/" {
			continue
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			continue
		}
		// makeDirIn never follows a link of the image: one to / must not put
		// a directory of the session over the session's own root.
		target, err := makeDirIn(reply.Root, dir)
		if err != nil {
			continue
		}
		if err := syscall.Mount(dir, target, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("show %s to %s: %w", dir, name, err)
		}
	}
	for _, file := range []string{"/etc/resolv.conf", "/etc/hosts"} {
		target, err := resolveIn(reply.Root, file)
		if err == nil {
			syscall.Mount(file, target, "", syscall.MS_BIND, "")
		}
	}
	if err := syscall.Chroot(reply.Root); err != nil {
		return fmt.Errorf("enter %s for %s: %w", reply.Image, name, err)
	}
	if err := syscall.Chdir(cwd); err != nil {
		syscall.Chdir("/")
	}

	env := map[string]string{"PATH": defaultPath}
	for k, v := range meta.Environment {
		env[k] = v
	}
	for _, key := range insideEnv {
		if v, ok := os.LookupEnv(key); ok {
			env[key] = v
		}
	}
	var list []string
	for k, v := range env {
		list = append(list, k+"="+v)
	}
	sort.Strings(list)
	program, err := lookPathInEnvironment(reply.Program, list)
	if err != nil {
		return fmt.Errorf("%s in %s: %w", name, reply.Image, err)
	}
	if err := checkExecutableArch(program); err != nil {
		return err
	}
	err = syscall.Exec(program, append([]string{filepath.Base(name)}, args...), list)
	return fmt.Errorf("run %s in %s: %w", program, reply.Image, err)
}
