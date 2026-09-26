package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/andreygrehov/range/internal/nbd"
	"github.com/andreygrehov/range/internal/profile"
	"github.com/andreygrehov/range/internal/session"
)

// commandGuest runs inside the Linux VM. It attaches the NBD export the host is
// serving and then follows exactly the same path as a native Linux session.
func commandGuest(args []string) error {
	fs := flag.NewFlagSet(session.GuestCommand, flag.ContinueOnError)
	endpoint := fs.String("nbd", "", "host:port of the NBD server on the host")
	sessionID := fs.String("session", "", "session id chosen by the host")
	readyPort := fs.Int("ready-port", 0, "host port to report readiness on")
	workload := fs.String("workload", profile.DefaultWorkload, "workload name")
	workdir := fs.String("workdir", "", "working directory inside the environment")
	shellPath := fs.String("shell", "", "shell to execute")
	guestFlags, guestRest := splitArgs(fs, args)
	if err := fs.Parse(guestFlags); err != nil {
		return err
	}
	if *endpoint == "" {
		return errors.New(session.GuestCommand + ": missing --nbd")
	}
	if os.Geteuid() != 0 {
		return errors.New(session.GuestCommand + ": must run as root inside the guest")
	}
	if _, _, err := net.SplitHostPort(*endpoint); err != nil {
		return err
	}
	device, err := session.AllocateNBDDevice("/sys/block", "/dev")
	if err != nil {
		return err
	}
	sess := &session.Session{ID: *sessionID, State: session.StateAttaching, StartedAt: time.Now()}
	if sess.ID == "" {
		sess.ID = session.NewID()
	}
	defer func() { _ = sess.Cleanup() }()
	if err := session.Prepare(sess, "", "", false); err != nil {
		return err
	}
	// The NBD client side runs here, in Go, so the guest needs no nbd-client.
	disconnect, err := nbd.ConnectKernel(*endpoint, device)
	if err != nil {
		return fmt.Errorf("attach %s: %w", device, err)
	}
	sess.Push(disconnect)
	if err := session.WaitForBlockDevice("/sys/block", device, 15*time.Second); err != nil {
		return err
	}
	opts := session.Options{
		Session: sess, Workload: *workload, Workdir: *workdir,
		ShellPath: *shellPath, Command: guestRest,
	}
	return session.MountAndRun(context.Background(), opts, device, func() {
		session.ReportReadyToHost(*readyPort)
	})
}
