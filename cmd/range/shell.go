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
	"github.com/andreygrehov/range/internal/profile"
	"github.com/andreygrehov/range/internal/session"
)

// commandRun is "shell" with a command: one non-interactive execution inside
// the environment, exiting with the workload's status. It is spelled
// separately because "range run env -- cmd" is the line a CI job or an agent
// writes, and it should not read as a shell that happens to take arguments.
func commandRun(args []string) error {
	for _, arg := range args {
		if arg == "--" {
			return commandShell(args)
		}
	}
	return errors.New("run: give the command after --, e.g. range run <uri> -- go test ./...")
}

// commandShell turns a remote artifact into a usable environment: attach it as
// a block device, mount it read-only, stack a writable overlay, and run a
// workload inside fresh namespaces. The kernel work is delegated to the runtime
// for this host; everything above it is identical everywhere.
func commandShell(args []string) error {
	started := time.Now()
	// Check the host before opening the artifact: no point paying for a HEAD
	// request against object storage if this machine cannot run an environment.
	host := session.Select()
	if err := session.CheckRequirements(host); err != nil {
		return err
	}
	var workdir, shellPath, upperDir, name, profileFlag, prefetchLimit, workload string
	var keep bool
	r, c, command, err := openFromArgs("shell", args, func(fs *flag.FlagSet) {
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
	r.Workload = workload

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
		if learned, ok := profile.Load(c.CacheDir, r.Ident, r.BlockSize, workload); ok {
			profileHit, priorSessions = true, learned.Sessions
			go r.PrefetchProfile(ctx, learned, c.PrefetchLimit)
		}
	}

	r.SessionID = sess.ID
	go r.PublishStats(5 * time.Second)

	opts := session.Options{
		Reader: r, Config: c, Session: sess, Workload: workload, Workdir: workdir,
		ShellPath: shellPath, Command: command, UpperDir: upperDir, EnvName: name, Keep: keep,
	}
	runErr := host.Run(ctx, opts, func() {
		sess.State = session.StateReady
		sess.ReadyIn = time.Since(started)
		r.ReadyIn, r.SessionName = sess.ReadyIn, sess.Name
		printEnvironmentBanner(sess, r, sess.Meta, profileHit, priorSessions)
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
	r.SessionName = sess.Name
	if err := r.WriteStats(); err != nil {
		log.Printf("range: could not save session statistics: %v", err)
	}
	if err := sess.Cleanup(); err != nil {
		log.Printf("range: cleanup: %v", err)
	}
	printSessionSummary(sess, r, sessionsLearned)
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

func printEnvironmentBanner(sess *session.Session, r *core.Reader, meta environment.Metadata, profileHit bool, priorSessions int64) {
	bannerLine("")
	bannerLine("Range environment")
	bannerLine("  Artifact          %s", r.Ident.URI)
	if meta.Name != "" {
		bannerLine("  Name              %s", meta.Name)
	}
	bannerLine("  Logical size      %s", bytesize.Format(r.Size()))
	bannerLine("  Runtime           %s/%s", runtime.GOOS, runtime.GOARCH)
	bannerLine("  Workload          %s", r.Workload)
	if profileHit {
		bannerLine("  Profile           %d prior sessions", priorSessions)
	} else {
		bannerLine("  Profile           none yet")
	}
	bannerLine("  Ready             %s", roundedSeconds(sess.ReadyIn))
	bannerLine("")
}

func printSessionSummary(sess *session.Session, r *core.Reader, sessionsLearned int64) {
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
