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
	"github.com/andreygrehov/range/internal/environment"
	"github.com/andreygrehov/range/internal/oci"
	"github.com/andreygrehov/range/internal/profile"
	"github.com/andreygrehov/range/internal/session"
)

// commandRun is "shell" with a command: one non-interactive execution inside
// the environment, exiting with the workload's status. It is spelled
// separately because "range run env -- cmd" is the line a CI job or an agent
// writes, and it should not read as a shell that happens to take arguments.
//
// With nothing after --, or no -- at all, it runs the environment's own
// command: a container image's entrypoint and cmd, as docker run does.
func commandRun(args []string) error {
	return runSession(args, true)
}

// commandShell turns a remote artifact into a usable environment: attach it as
// a block device, mount it read-only, stack a writable overlay, and run a
// workload inside fresh namespaces. The kernel work is delegated to the runtime
// for this host; everything above it is identical everywhere.
func commandShell(args []string) error {
	return runSession(args, false)
}

// runSession is shell and run: defaultCommand says what an empty command means.
func runSession(args []string, defaultCommand bool) error {
	started := time.Now()
	// Check the host before opening the artifact: no point paying for a HEAD
	// request against object storage if this machine cannot run an environment.
	host := session.Select()
	if err := session.CheckRequirements(host); err != nil {
		return err
	}
	var workdir, shellPath, upperDir, name, profileFlag, prefetchLimit, workload string
	var keep, gpus bool
	var mountSpecs, env []string
	var ports []session.Port
	r, c, command, err := openFromArgs("shell", args, func(fs *flag.FlagSet) {
		fs.Func("mount", "SOURCE:/path: a remote filesystem, read-only, or a directory of this machine, read-write (repeatable)", func(v string) error {
			if _, _, err := session.ParseMount(v); err != nil {
				return err
			}
			mountSpecs = append(mountSpecs, v)
			return nil
		})
		publish := func(v string) error {
			p, err := session.ParsePort(v)
			ports = append(ports, p)
			return err
		}
		fs.Func("publish", "[IP:]HOSTPORT:PORT, publish a port of the environment here (repeatable)", publish)
		fs.Func("p", "short for --publish", publish)
		setEnv := func(v string) error {
			key, value, ok, err := session.ParseEnv(v)
			if ok {
				env = append(env, key+"="+value)
			}
			return err
		}
		fs.Func("env", "KEY=VALUE, or KEY to pass this shell's value (repeatable)", setEnv)
		fs.Func("e", "short for --env", setEnv)
		fs.Func("gpus", "all: show this machine's NVIDIA GPUs inside the environment", func(v string) error {
			gpus = true
			return session.ParseGPUs(v)
		})
		fs.StringVar(&workload, "workload", profile.DefaultWorkload, "workload name, so profiles do not collide")
		fs.StringVar(&workdir, "workdir", "", "working directory inside the environment")
		fs.StringVar(&shellPath, "shell", "", "shell to execute")
		fs.StringVar(&upperDir, "upper-dir", "", "directory holding the writable layer")
		fs.StringVar(&name, "name", "", "named environment whose writes survive exit")
		fs.BoolVar(&keep, "keep", false, "keep this session's writable layer")
		fs.StringVar(&profileFlag, "profile", "", "off, record or auto")
		fs.StringVar(&prefetchLimit, "prefetch-limit", "", "ceiling on profile prefetch, e.g. 256MiB")
	})
	if err != nil {
		return err
	}
	defer r.Close()
	if profileFlag != "" {
		mode, err := core.ParseProfileMode(profileFlag)
		if err != nil {
			return err
		}
		c.ProfileMode = mode
	}
	if prefetchLimit != "" {
		limit, err := bytesize.Parse(prefetchLimit)
		if err != nil {
			return err
		}
		c.PrefetchLimit = limit
	}
	if workload == "" {
		workload = profile.DefaultWorkload
	}
	var nvidia *session.NVIDIA
	if gpus {
		if host.Name() != "native" {
			return errors.New(gpuHint())
		}
		if nvidia, err = session.FindNVIDIA(c.CacheDir); err != nil {
			return err
		}
	}
	r.Workload = workload

	// Each mount is a reader of its own, with its own cache and profile.
	var mounts []session.Mount
	var dirs []session.Dir
	for _, spec := range mountSpecs {
		uri, target, _ := session.ParseMount(spec)
		if path, ok := session.LocalDir(uri); ok {
			dirs = append(dirs, session.Dir{Path: path, Target: target})
			continue
		}
		mr, err := core.Open(context.Background(), uri, c)
		if err != nil {
			return fmt.Errorf("--mount %s: %w", uri, err)
		}
		defer mr.Close()
		mr.Workload = workload
		mounts = append(mounts, session.Mount{Target: target, Reader: mr})
	}

	sess := &session.Session{ID: session.NewID(), State: session.StateCreating, StartedAt: started}
	defer func() { _ = sess.Cleanup() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Ctrl+C belongs to the workload. Catch SIGINT rather than ignoring it: an
	// ignored signal survives exec and would leave an uninterruptible shell.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		for {
			select {
			case sig := <-signals:
				if sig == syscall.SIGTERM {
					cancel()
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Replay a previous session's working set. This must never delay startup.
	profileHit := false
	var priorSessions int64
	if c.ProfileMode == core.ProfileAuto {
		learned, ok := profile.Load(c.CacheDir, r.Ident, r.BlockSize, workload)
		if !ok {
			learned, ok = catalogProfile(ctx, r, c, workload)
		}
		if ok {
			profileHit, priorSessions = true, learned.Sessions
			go r.PrefetchProfile(ctx, learned, c.PrefetchLimit)
		}
		for _, m := range mounts {
			if learned, ok := profile.Load(c.CacheDir, m.Reader.Ident, m.Reader.BlockSize, workload); ok {
				go m.Reader.PrefetchProfile(ctx, learned, c.PrefetchLimit)
			}
		}
	}

	r.SessionID = sess.ID
	go r.PublishStats(5 * time.Second)

	opts := session.Options{
		Reader: r, Config: c, Session: sess, Workload: workload, Workdir: workdir,
		ShellPath: shellPath, Command: command, UpperDir: upperDir, EnvName: name, Keep: keep,
		Mounts: mounts, Dirs: dirs, Env: env, Ports: ports, DefaultCommand: defaultCommand,
		NVIDIA: nvidia,
	}
	runErr := host.Run(ctx, opts, func() {
		sess.State = session.StateReady
		sess.ReadyIn = time.Since(started)
		r.ReadyIn, r.SessionName = sess.ReadyIn, sess.Name
		printEnvironmentBanner(sess, r, sess.Meta, profileHit, priorSessions, mounts, dirs, nvidia)
	})

	// Record what this session needed before tearing anything down.
	sessionsLearned := priorSessions
	if c.ProfileMode != core.ProfileOff && r.Recorder.Count() > 0 {
		merged, err := profile.Save(c.CacheDir, r.Ident, r.BlockSize, workload,
			r.Recorder.Observations(), time.Now())
		if err != nil {
			log.Printf("range: could not save working-set profile: %v", err)
		} else {
			sessionsLearned = merged.Sessions
		}
	}
	for _, m := range mounts {
		if c.ProfileMode != core.ProfileOff && m.Reader.Recorder.Count() > 0 {
			if _, err := profile.Save(c.CacheDir, m.Reader.Ident, m.Reader.BlockSize, workload,
				m.Reader.Recorder.Observations(), time.Now()); err != nil {
				log.Printf("range: could not save the profile of %s: %v", m.Reader.Ident.URI, err)
			}
		}
		m.Reader.SessionName = sess.Name
		if err := m.Reader.WriteStats(); err != nil {
			log.Printf("range: could not save statistics of %s: %v", m.Reader.Ident.URI, err)
		}
	}
	r.SessionName = sess.Name
	if err := r.WriteStats(); err != nil {
		log.Printf("range: could not save session statistics: %v", err)
	}
	if err := sess.Cleanup(); err != nil {
		log.Printf("range: cleanup: %v", err)
	}
	printSessionSummary(sess, r, sessionsLearned, mounts)
	return runErr
}

// bannerLine prints one line of the startup banner.
//
// On macOS the banner is written while ssh holds the local terminal in raw mode
// for the guest's pty, and there a bare newline moves down a row without
// returning the carriage, so the banner comes out as a staircase. Ending each
// line with CRLF is correct in raw mode and invisible in cooked mode. A pipe or
// a file still gets plain newlines so captured output stays clean.
func bannerLine(format string, args ...any) {
	ending := "\n"
	if info, err := os.Stdout.Stat(); err == nil && info.Mode()&os.ModeCharDevice != 0 {
		ending = "\r\n"
	}
	fmt.Printf(format+ending, args...)
}

func printEnvironmentBanner(sess *session.Session, r *core.Reader, meta environment.Metadata, profileHit bool, priorSessions int64, mounts []session.Mount, dirs []session.Dir, nvidia *session.NVIDIA) {
	bannerLine("")
	bannerLine("Range environment")
	bannerLine("  Artifact          %s", r.Ident.URI)
	if meta.Name != "" {
		bannerLine("  Name              %s", meta.Name)
	}
	bannerLine("  Logical size      %s", bytesize.Format(r.Size()))
	for _, m := range mounts {
		bannerLine("  Mounted           %s at %s (%s)", m.Reader.Ident.URI, m.Target, bytesize.Format(m.Reader.Size()))
	}
	for _, d := range dirs {
		bannerLine("  Shared            %s at %s", d.Path, d.Target)
	}
	bannerLine("  Runtime           %s/%s", runtime.GOOS, runtime.GOARCH)
	if nvidia != nil {
		bannerLine("  GPUs              %d NVIDIA, driver %s", nvidia.GPUs, nvidia.Version)
	}
	bannerLine("  Workload          %s", r.Workload)
	if profileHit {
		bannerLine("  Profile           %d prior sessions", priorSessions)
	} else {
		bannerLine("  Profile           none yet")
	}
	bannerLine("  Ready             %s", roundedSeconds(sess.ReadyIn))
	bannerLine("")
}

func printSessionSummary(sess *session.Session, r *core.Reader, sessionsLearned int64, mounts []session.Mount) {
	s := r.Snapshot()
	fmt.Println()
	fmt.Println("Range session complete")
	fmt.Println()
	fmt.Printf("  Artifact            %s\n", bytesize.Format(s.Size))
	fmt.Printf("  Working set         %s\n", bytesize.Format(s.WorkingSetBytes))
	fmt.Printf("  Working-set ratio   %.4f%%\n", s.WorkingSetRatio())
	fmt.Printf("  Remote transfer     %s\n", bytesize.Format(s.SessionRemoteBytes))
	fmt.Printf("  Requests            %d\n", s.SessionRequests)
	if s.SessionCompressedBytes > 0 {
		fmt.Printf("  Remote compressed   %s over %d requests\n",
			bytesize.Format(s.SessionCompressedBytes), s.SessionCompressedReqs)
		fmt.Printf("  Network amplification %.2fx of the working set\n",
			float64(s.SessionCompressedBytes)/float64(s.WorkingSetBytes))
	}
	fmt.Printf("  Fetch amplification %.2fx\n", s.Amplification())
	for _, m := range mounts {
		ms := m.Reader.Snapshot()
		fmt.Printf("  %-19s %s read, %s transferred, of %s\n", m.Target, bytesize.Format(ms.WorkingSetBytes),
			bytesize.Format(ms.SessionRemoteBytes), bytesize.Format(ms.Size))
	}
	if sessionsLearned > 0 {
		fmt.Printf("  Profile sessions    %d\n", sessionsLearned)
	}
	fmt.Printf("  Runtime             %s\n", roundedSeconds(time.Since(sess.StartedAt)))
	if sess.Persistent {
		fmt.Printf("  Writable layer      %s\n", sess.Upper)
	}
	fmt.Println()
}

func roundedSeconds(d time.Duration) string {
	return fmt.Sprintf("%.2f s", d.Seconds())
}

// catalogProfile fetches the startup profile the catalog holds for an image
// this host has never run, so even a first session fetches what the shell and
// the command need at once instead of one read after another. It is used only
// for the same image, layout, size, block size and workload, and kept as this
// host's own profile, which later sessions add to.
func catalogProfile(ctx context.Context, r *core.Reader, c core.Config, workload string) (profile.Profile, bool) {
	data, err := oci.FetchCatalogProfile(ctx, r.Ident.ETag)
	if err != nil {
		return profile.Profile{}, false
	}
	p, err := profile.Parse(data)
	if err != nil || p.Artifact.ETag != r.Ident.ETag || p.Artifact.Size != r.Ident.Size ||
		p.Artifact.VersionID != "" || p.BlockSize != r.BlockSize || p.Workload != workload {
		return profile.Profile{}, false
	}
	p.Artifact.URI = r.Ident.URI // the catalog's spelling of the image may differ from this one
	if err := profile.WriteFile(profile.Path(c.CacheDir, r.Ident, workload), p); err != nil {
		return profile.Profile{}, false
	}
	return profile.Load(c.CacheDir, r.Ident, r.BlockSize, workload)
}

// gpuHint says how to get GPUs where the runtime cannot show them: only the
// native runtime can, since Range's VMs have no GPU.
func gpuHint() string {
	if runtime.GOOS == "linux" {
		return "--gpus needs the native runtime, which runs as root: re-run with sudo"
	}
	return "--gpus needs Linux: Range's VM on a Mac has no GPU"
}
