package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/nbd"
	"github.com/andreygrehov/range/internal/profile"
	"github.com/andreygrehov/range/internal/session"
	"github.com/andreygrehov/range/internal/tool"
)

// commandMount attaches a remote filesystem as a block device and mounts it
// read-only on a directory of this host, until Ctrl+C. It is the shell without
// the environment: the files are there for any process to read, and only the
// blocks those processes touch cross the network.
func commandMount(args []string) error {
	started := time.Now()
	if runtime.GOOS != "linux" {
		return errors.New("mount: needs Linux; on macOS, open the environment with range shell")
	}
	if os.Geteuid() != 0 {
		return errors.New("mount: needs root to attach a block device; re-run with sudo")
	}
	var workload, profileFlag, prefetchLimit, readahead string
	r, c, rest, err := openFromArgs("mount", args, func(fs *flag.FlagSet) {
		fs.StringVar(&workload, "workload", "mount", "workload name, so profiles do not collide")
		fs.StringVar(&profileFlag, "profile", "", "off, record or auto")
		fs.StringVar(&prefetchLimit, "prefetch-limit", "", "ceiling on profile prefetch, e.g. 1GiB")
		fs.StringVar(&readahead, "readahead", "16MiB", "kernel readahead on the device, see session.MountReadahead")
	})
	if err != nil {
		return err
	}
	defer r.Close()
	if profileFlag != "" {
		if c.ProfileMode, err = core.ParseProfileMode(profileFlag); err != nil {
			return err
		}
	}
	if prefetchLimit != "" {
		if c.PrefetchLimit, err = bytesize.Parse(prefetchLimit); err != nil {
			return err
		}
	}
	readaheadBytes, err := bytesize.Parse(readahead)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("mount: want range mount <uri> <dir>")
	}
	dir := rest[0]
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	r.Workload = workload

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	device, err := session.AllocateNBDDevice("/sys/block", "/dev")
	if err != nil {
		return err
	}
	attachCtx, stopAttach := context.WithCancel(context.Background())
	var attachErr error
	attached := make(chan struct{})
	go func() {
		attachErr = nbd.Attach(attachCtx, device, r, r.Size())
		close(attached)
	}()
	detach := func() error {
		stopAttach()
		select {
		case <-attached:
			return attachErr
		case <-time.After(10 * time.Second):
			return fmt.Errorf("%s did not disconnect", device)
		}
	}
	ready := make(chan error, 1)
	go func() { ready <- session.WaitForBlockDevice("/sys/block", device, 15*time.Second) }()
	select {
	case <-attached:
		return fmt.Errorf("attach %s: %w", device, attachErr)
	case err := <-ready:
		if err != nil {
			_ = detach()
			return err
		}
	}

	// A large readahead suits programs that map files; see session.MountReadahead.
	if err := session.SetReadahead(device, readaheadBytes); err != nil {
		log.Printf("range: could not set readahead on %s: %v", device, err)
	}

	// Replay what earlier mounts of this object read. It never delays the mount.
	learned, profileHit := profile.Profile{}, false
	if c.ProfileMode == core.ProfileAuto {
		learned, profileHit = profile.Load(c.CacheDir, r.Ident, r.BlockSize, workload)
		if profileHit {
			go r.PrefetchProfile(ctx, learned, c.PrefetchLimit)
		}
	}
	// Name the filesystem: left to probe, mount reads superblock locations all
	// over the device, and on a remote device each of those is a request.
	fsType, err := session.DetectFilesystem(device)
	if err != nil {
		_ = detach()
		return err
	}
	// nodev and nosuid: the files come from someone else's image, and a
	// setuid binary or a device node in it must not act on this host.
	if err := tool.Run("mount", "-t", fsType, "-o", "ro,nodev,nosuid", device, dir); err != nil {
		_ = detach()
		return fmt.Errorf("mount %s on %s: %w", device, dir, err)
	}
	go r.PublishStats(5 * time.Second)
	fmt.Printf("%s mounted read-only on %s\n", r.Ident.URI, dir)
	fmt.Printf("  Size       %s\n", bytesize.Format(r.Size()))
	fmt.Printf("  Ready      %.2f s\n", time.Since(started).Seconds())
	if profileHit {
		fmt.Printf("  Profile    learned from %d earlier mounts, prefetching\n", learned.Sessions)
	}
	fmt.Println("Press Ctrl+C to unmount.")

	select {
	case <-ctx.Done():
	case <-attached:
		log.Printf("range: %s detached on its own: %v", device, attachErr)
	}

	var errs []error
	if err := tool.Run("umount", dir); err != nil {
		// A process still has files open. Detach the mount lazily rather
		// than leave the device attached forever.
		if lazyErr := tool.Run("umount", "-l", dir); lazyErr != nil {
			errs = append(errs, fmt.Errorf("unmount %s: %w (is a process still using it?)", dir, err))
		}
	}
	if c.ProfileMode != core.ProfileOff && r.Recorder.Count() > 0 {
		if _, err := profile.Save(c.CacheDir, r.Ident, r.BlockSize, workload,
			r.Recorder.Observations(), time.Now()); err != nil {
			log.Printf("range: could not save working-set profile: %v", err)
		}
	}
	if err := r.WriteStats(); err != nil {
		log.Printf("range: could not save statistics: %v", err)
	}
	if err := detach(); err != nil {
		errs = append(errs, err)
	}
	printStats(r.Snapshot())
	return errors.Join(errs...)
}
