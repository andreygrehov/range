package session

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/tool"
)

// A session runs a command the environment does not have from the image that
// holds it: in range shell ubuntu, nvcc runs from nvidia/cuda. Each command
// of the catalog's map has a shim in /.range/shims, at the end of PATH, so
// whatever the environment has comes first. A shim asks the session, through
// a socket, for its command's image. The session finds it in the map, opens
// it lazily, attaches it and mounts it in the session's own mount namespace,
// under /.range/images. The shim then runs the command in that image's root,
// where the session's current directory, /tmp and home are at their own
// paths. It stays a process of the shell that started it, so pipes, Ctrl+C,
// job control and the exit status are the shell's own.

const (
	insideDir = "/.range"
	// InsideSocket is where a shim asks the session for its command.
	InsideSocket = insideDir + "/control.sock"
	// InsideShims holds a shim for each command of the map.
	InsideShims = insideDir + "/shims"
	// insideRange is range itself, which every shim links to.
	insideRange = insideDir + "/bin/range"
)

// Finder is what a session needs of the command map.
type Finder interface {
	// Names lists the commands a session can run from other images.
	Names(ctx context.Context) ([]string, error)
	// Find names the image that holds command, with version as its tag or
	// else the usual one, and the program to run in it.
	Find(ctx context.Context, command, version string) (image, program string, err error)
}

// InsideRequest is what a shim asks for.
type InsideRequest struct {
	Command string `json:"command"`
	Version string `json:"version,omitempty"`
}

// InsideReply is where the command's image is mounted in the session: Root
// to run it in, and Lower, the image as it is, to read its metadata from.
type InsideReply struct {
	Image   string `json:"image,omitempty"`
	Root    string `json:"root,omitempty"`
	Lower   string `json:"lower,omitempty"`
	Program string `json:"program,omitempty"`
	// First says this is the session's first command from the image.
	First bool   `json:"first,omitempty"`
	Error string `json:"error,omitempty"`
}

// inside serves the shims of one session.
type inside struct {
	sess   *Session
	opts   Options
	finder Finder

	pid     chan struct{} // closed once nsPID is the session's namespaces
	nsPID   int
	mu      sync.Mutex
	images  map[string]*insideImage
	counter int
}

type insideImage struct {
	ready       chan struct{}
	root, lower string
	err         error
	given       bool
}

// startInside lays out /.range in the session's root: range itself, the
// shims, and the socket the shims ask on. It must run before the session's
// namespaces exist, which then share it.
func startInside(ctx context.Context, sess *Session, opts Options) (*inside, error) {
	dir, err := makeDirIn(sess.root, insideDir)
	if err != nil {
		return nil, err
	}
	if err := tool.Run("mount", "-t", "tmpfs", "-o", "mode=755,nosuid,nodev", "range", dir); err != nil {
		return nil, err
	}
	sess.Push(func() error { return tool.Run("umount", "-l", dir) })
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(bin, "range"), nil, 0o755); err != nil {
		return nil, err
	}
	if err := tool.Run("mount", "--bind", "-o", "ro", self, filepath.Join(bin, "range")); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", filepath.Join(dir, filepath.Base(InsideSocket)))
	if err != nil {
		return nil, err
	}
	os.Chmod(filepath.Join(dir, filepath.Base(InsideSocket)), 0o666)
	sess.Push(listener.Close)
	in := &inside{sess: sess, opts: opts, finder: opts.Commands, pid: make(chan struct{}),
		images: map[string]*insideImage{}}
	go in.makeShims(ctx, dir)
	go in.serve(ctx, listener)
	return in, nil
}

// started records the process whose namespaces are the session's.
func (in *inside) started(pid int) {
	in.nsPID = pid
	close(in.pid)
}

// makeShims links every command of the map to range, then swaps the links
// in whole. It runs while the session starts, so a session never waits for
// it.
func (in *inside) makeShims(ctx context.Context, dir string) {
	names, err := in.finder.Names(ctx)
	if err != nil {
		return
	}
	fresh := filepath.Join(dir, ".shims")
	if os.Mkdir(fresh, 0o755) != nil {
		return
	}
	for _, name := range names {
		os.Symlink(insideRange, filepath.Join(fresh, name))
	}
	os.Rename(fresh, filepath.Join(dir, filepath.Base(InsideShims)))
}

func (in *inside) serve(ctx context.Context, listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			var req InsideRequest
			if err := json.NewDecoder(conn).Decode(&req); err != nil {
				return
			}
			reply := in.handle(ctx, req)
			json.NewEncoder(conn).Encode(reply)
		}()
	}
}

func (in *inside) handle(ctx context.Context, req InsideRequest) InsideReply {
	image, program, err := in.finder.Find(ctx, req.Command, req.Version)
	if err != nil {
		return InsideReply{Error: err.Error()}
	}
	select {
	case <-in.pid:
	case <-ctx.Done():
		return InsideReply{Error: ctx.Err().Error()}
	}
	in.mu.Lock()
	img, ok := in.images[image]
	if !ok {
		img = &insideImage{ready: make(chan struct{})}
		in.images[image] = img
		in.counter++
		n := in.counter
		in.mu.Unlock()
		img.root, img.lower, img.err = in.mount(ctx, image, n)
		close(img.ready)
	} else {
		in.mu.Unlock()
		<-img.ready
	}
	if img.err != nil {
		return InsideReply{Error: img.err.Error()}
	}
	in.mu.Lock()
	first := !img.given
	img.given = true
	in.mu.Unlock()
	return InsideReply{Image: image, Root: img.root, Lower: img.lower, Program: program, First: first}
}

// attach opens image and attaches it as a block device, natively or as the
// session's runtime does.
func (in *inside) attach(ctx context.Context, image string) (string, error) {
	if open := in.opts.OpenImage; open != nil {
		device, detach, err := open(ctx, image)
		if err != nil {
			return "", err
		}
		in.sess.Push(detach)
		return device, nil
	}
	r, err := core.Open(ctx, image, in.opts.Config)
	if err != nil {
		return "", err
	}
	in.sess.Push(func() error { r.Close(); return nil })
	return attach(in.sess, r)
}

// mount opens image, attaches it and mounts it in the session's namespace:
// read-only, with a layer of the session's own on top, so a command can write
// in its image's root, and nothing of it outlives the session.
func (in *inside) mount(ctx context.Context, image string, n int) (root, lower string, err error) {
	device, err := in.attach(ctx, image)
	if err != nil {
		return "", "", err
	}
	fsType, err := DetectFilesystem(device)
	if err != nil {
		return "", "", fmt.Errorf("%s: %w", image, err)
	}
	if err := loadFilesystem(in.opts.LoadFilesystem, fsType); err != nil {
		return "", "", err
	}
	base := filepath.Join(insideDir, "images", strconv.Itoa(n))
	host := filepath.Join(in.sess.root, base)
	for _, d := range []string{"lower", "upper", "work", "root"} {
		if err := os.MkdirAll(filepath.Join(host, d), 0o755); err != nil {
			return "", "", err
		}
	}
	// The session's namespace has its own mounts: these go there, not here.
	ns := []string{"--target", strconv.Itoa(in.nsPID), "--mount", "--", "mount"}
	if err := tool.Run("nsenter", append(ns, "-t", fsType, "-o", readOnly(fsType), device, filepath.Join(host, "lower"))...); err != nil {
		return "", "", fmt.Errorf("mount %s: %w", image, err)
	}
	options := overlayOptions(filepath.Join(host, "lower"), filepath.Join(host, "upper"), filepath.Join(host, "work"))
	if err := tool.Run("nsenter", append(ns, "-t", "overlay", "overlay", "-o", options, filepath.Join(host, "root"))...); err != nil {
		return "", "", fmt.Errorf("mount %s: %w", image, err)
	}
	return filepath.Join(base, "root"), filepath.Join(base, "lower"), nil
}
