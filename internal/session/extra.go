package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/tool"
)

// MountReadahead is the kernel readahead given to a mounted filesystem's
// device. Programs that map a file, as model loaders do, fault it in a page at
// a time and out of order; the kernel reads around each fault by this much, so
// thousands of small remote reads become a few large ones. The kernel's own
// default, 128 KiB, suits a local disk.
const MountReadahead = 16 << 20

// Mount is a second remote filesystem a session shows read-only inside the
// environment, at Target. The side that serves it holds the Reader; the side
// that attaches it fills in Device.
type Mount struct {
	Target string
	Reader *core.Reader
	Device string
}

// Dir is a directory of this machine shown read-write inside the
// environment, at Target, the way docker run -v shows one.
type Dir struct {
	Path   string
	Target string
}

// LocalDir reports whether the source of a --mount is a directory on this
// machine, and returns its absolute path. A URI, or a file such as a local
// .range artifact, is not one.
func LocalDir(source string) (string, bool) {
	if strings.Contains(source, "://") {
		return "", false
	}
	if rest, ok := strings.CutPrefix(source, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		source = filepath.Join(home, rest)
	}
	info, err := os.Stat(source)
	if err != nil || !info.IsDir() {
		return "", false
	}
	abs, err := filepath.Abs(source)
	return abs, err == nil
}

// bindDirs shows each directory inside the environment, read-write.
func bindDirs(sess *Session, dirs []Dir) error {
	for _, d := range dirs {
		target, err := makeDirIn(sess.root, d.Target)
		if err != nil {
			return err
		}
		if err := tool.Run("mount", "--bind", d.Path, target); err != nil {
			return fmt.Errorf("show %s at %s: %w", d.Path, d.Target, err)
		}
		sess.Push(func() error {
			if err := tool.Run("umount", target); err != nil {
				return tool.Run("umount", "-l", target)
			}
			return nil
		})
	}
	return nil
}

// ParseMount splits "URI:/path" at the last ":/", so a URI's own scheme
// separator stays with the URI.
func ParseMount(spec string) (uri, target string, err error) {
	at := strings.LastIndex(spec, ":/")
	if at <= 0 {
		return "", "", fmt.Errorf("--mount %q: want SOURCE:/path, e.g. hf://org/model:/models or .:/work", spec)
	}
	uri, target = spec[:at], spec[at+1:]
	if err := CheckMountTarget(target); err != nil {
		return "", "", fmt.Errorf("--mount %q: %w", spec, err)
	}
	return uri, target, nil
}

// CheckMountTarget accepts a clean absolute path other than the root.
func CheckMountTarget(target string) error {
	if !filepath.IsAbs(target) || filepath.Clean(target) != target || target == "/" {
		return fmt.Errorf("mount target %q must be a clean absolute path below /", target)
	}
	return nil
}

// SetReadahead sets a block device's readahead.
func SetReadahead(device string, bytes int64) error {
	return os.WriteFile(filepath.Join("/sys/block", filepath.Base(device), "queue", "read_ahead_kb"),
		[]byte(strconv.FormatInt(bytes>>10, 10)), 0o644)
}

// makeDirIn creates target below root one component at a time and refuses to
// follow a symlink. The environment's own files decide what the components
// are, and a /models that links to / must not put a mount on the host's root.
func makeDirIn(root, target string) (string, error) {
	if err := CheckMountTarget(target); err != nil {
		return "", err
	}
	path := root
	for _, name := range strings.Split(strings.TrimPrefix(target, "/"), "/") {
		path = filepath.Join(path, name)
		info, err := os.Lstat(path)
		switch {
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(path, 0o755); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case info.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("cannot mount on %s: %s is a symlink in the environment",
				target, strings.TrimPrefix(path, root))
		case !info.IsDir():
			return "", fmt.Errorf("cannot mount on %s: %s is not a directory in the environment",
				target, strings.TrimPrefix(path, root))
		}
	}
	return path, nil
}

// mountExtras mounts every attached Mount read-only inside the session root.
// Each is unmounted before the root is, because cleanups run in reverse.
func mountExtras(sess *Session, mounts []Mount) error {
	for _, m := range mounts {
		if err := SetReadahead(m.Device, MountReadahead); err != nil {
			return fmt.Errorf("set readahead on %s: %w", m.Device, err)
		}
		fsType, err := DetectFilesystem(m.Device)
		if err != nil {
			return fmt.Errorf("%s: %w", m.Target, err)
		}
		dir, err := makeDirIn(sess.root, m.Target)
		if err != nil {
			return err
		}
		if err := tool.Run("mount", "-t", fsType, "-o", "ro,nodev,nosuid", m.Device, dir); err != nil {
			return fmt.Errorf("mount %s on %s: %w", m.Device, m.Target, err)
		}
		sess.Push(func() error {
			if err := tool.Run("umount", dir); err != nil {
				// A process that outlived the shell can hold it; detach lazily
				// rather than leave the device attached forever.
				return tool.Run("umount", "-l", dir)
			}
			return nil
		})
	}
	return nil
}
