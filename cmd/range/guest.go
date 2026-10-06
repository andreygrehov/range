package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"strings"
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
	defaultCommand := fs.Bool("default-command", false, "with no command, run the environment's own")
	var env []string
	fs.Func("env", "KEY=VALUE to set in the environment (repeatable)", func(v string) error {
		env = append(env, v)
		return nil
	})
	var mounts []session.Mount
	var endpoints []string
	fs.Func("mount-nbd", "TARGET=host:port of a further filesystem to mount (repeatable)", func(v string) error {
		target, endpoint, ok := strings.Cut(v, "=")
		if !ok {
			return fmt.Errorf("want TARGET=host:port, got %q", v)
		}
		if err := session.CheckMountTarget(target); err != nil {
			return err
		}
		if _, _, err := net.SplitHostPort(endpoint); err != nil {
			return err
		}
		mounts = append(mounts, session.Mount{Target: target})
		endpoints = append(endpoints, endpoint)
		return nil
	})
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
	if err := session.LoadNBD(); err != nil {
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
	if err := connectGuestDevice(sess, *endpoint, device); err != nil {
		return err
	}
	for i := range mounts {
		if mounts[i].Device, err = session.AllocateNBDDevice("/sys/block", "/dev"); err != nil {
			return err
		}
		if err := connectGuestDevice(sess, endpoints[i], mounts[i].Device); err != nil {
			return err
		}
	}
	opts := session.Options{
		Session: sess, Workload: *workload, Workdir: *workdir,
		ShellPath: *shellPath, Command: guestRest, Mounts: mounts, Env: env, DefaultCommand: *defaultCommand,
	}
	// The session ends with range on the host, however that ends.
	ctx, hostGone := context.WithCancel(context.Background())
	defer hostGone()
	return session.MountAndRun(ctx, opts, device, func() {
		session.ReportReadyToHost(*readyPort, hostGone)
	})
}

// connectGuestDevice connects device to the host's NBD export at endpoint and
// waits until the kernel has it. The disconnect joins the session's cleanups.
func connectGuestDevice(sess *session.Session, endpoint, device string) error {
	disconnect, err := nbd.ConnectKernel(endpoint, device)
	if err != nil {
		return fmt.Errorf("attach %s: %w", device, err)
	}
	sess.Push(disconnect)
	return session.WaitForBlockDevice("/sys/block", device, 15*time.Second)
}
