package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/andreygrehov/range/internal/session"
	"github.com/andreygrehov/range/internal/vm"
)

// The commands that reach a running session, spelled as docker's are:
//
//	range exec [-i] [-t] [-e K=V] [-w DIR] [-u UID[:GID]] SESSION COMMAND [ARG...]
//	range cp SRC SESSION:DEST, range cp SESSION:SRC DEST ("-" is a tar stream)
//	range ps [-q]
//	range stop [-t SECONDS] SESSION...
//	range logs [-f] SESSION
//
// SESSION is an ID, a prefix of one, or the name a session was given.

func commandExec(args []string) error {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	var interactive, tty bool
	var workdir string
	var env []string
	var user *session.User
	fs.BoolVar(&interactive, "interactive", false, "keep standard input open")
	fs.BoolVar(&interactive, "i", false, "short for --interactive")
	fs.BoolVar(&tty, "tty", false, "give the command a terminal")
	fs.BoolVar(&tty, "t", false, "short for --tty")
	fs.StringVar(&workdir, "workdir", "", "directory to run the command in")
	fs.StringVar(&workdir, "w", "", "short for --workdir")
	setEnv := func(v string) error {
		key, value, ok, err := session.ParseEnv(v)
		if ok {
			env = append(env, key+"="+value)
		}
		return err
	}
	fs.Func("env", "KEY=VALUE, or KEY to pass this shell's value (repeatable)", setEnv)
	fs.Func("e", "short for --env", setEnv)
	setUser := func(v string) (err error) {
		user, err = session.ParseUser(v)
		return err
	}
	fs.Func("user", "UID[:GID] to run the command as, by number", setUser)
	fs.Func("u", "short for --user", setUser)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: range exec [-i] [-t] [-e KEY=VALUE] [-w DIR] [-u UID[:GID]] SESSION COMMAND [ARG...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(combinedShortFlags(args)); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) > 1 && rest[1] == "--" {
		rest = append(rest[:1], rest[2:]...)
	}
	if len(rest) < 2 {
		fs.Usage()
		return errors.New("exec: give a session and a command")
	}
	rec, err := session.Find(rest[0])
	if err != nil {
		return err
	}
	req := session.Request{Op: "exec", Argv: rest[1:], Env: env, Workdir: workdir, User: user,
		TTY: tty, Interactive: interactive}
	terminal := tty && session.IsTerminal(os.Stdin) && session.IsTerminal(os.Stdout)
	if terminal {
		req.Rows, req.Cols, _ = vm.TerminalSize(int(os.Stdout.Fd()))
	}
	conn, err := session.Dial(rec.ID, req)
	if err != nil {
		return err
	}
	defer conn.Close()
	frames := session.NewFrameWriter(conn)

	if terminal && interactive {
		restore := vm.RawTerminal(int(os.Stdin.Fd()))
		defer restore()
	}
	if interactive {
		go func() {
			frames.Stream(session.FrameStdin, os.Stdin)
			frames.Write(session.FrameStdin, nil)
		}()
	} else {
		frames.Write(session.FrameStdin, nil)
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	defer signal.Stop(signals)
	go func() {
		for sig := range signals {
			if sig == syscall.SIGWINCH {
				if rows, cols, ok := vm.TerminalSize(int(os.Stdout.Fd())); ok {
					frames.Write(session.FrameResize, []byte(fmt.Sprintf("%d %d", rows, cols)))
				}
				continue
			}
			frames.Write(session.FrameSignal, []byte(strconv.Itoa(int(sig.(syscall.Signal)))))
		}
	}()
	code, err := readOutput(conn)
	if err != nil {
		return err
	}
	if code != 0 {
		return session.ExitStatus(code)
	}
	return nil
}

// readOutput copies a request's output here until its exit status.
func readOutput(conn net.Conn) (int, error) {
	for {
		kind, data, err := session.ReadFrame(conn)
		if err != nil {
			return 0, errors.New("the session closed the connection: it may have ended")
		}
		switch kind {
		case session.FrameStdout:
			os.Stdout.Write(data)
		case session.FrameStderr:
			os.Stderr.Write(data)
		case session.FrameExit:
			return strconv.Atoi(string(data))
		case session.FrameError:
			return 0, errors.New(string(data))
		}
	}
}

// combinedShortFlags splits -it into -i -t, as docker reads it, up to the
// first argument that is not a flag.
func combinedShortFlags(args []string) []string {
	var out []string
	for i, a := range args {
		if len(a) > 2 && a[0] == '-' && a[1] != '-' && strings.Trim(a[1:], "itd") == "" {
			for _, c := range a[1:] {
				out = append(out, "-"+string(c))
			}
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "--" {
			return append(out, args[i:]...)
		}
		out = append(out, a)
	}
	return out
}

func commandCp(args []string) error {
	fs := flag.NewFlagSet("cp", flag.ContinueOnError)
	follow := fs.Bool("L", false, "follow a link as SRC: copy what it points to")
	fs.BoolVar(follow, "follow-link", false, "follow a link as SRC: copy what it points to")
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	if len(args) != 2 {
		return errors.New("usage: range cp SRC SESSION:DEST, or range cp SESSION:SRC DEST; - is a tar stream")
	}
	srcSession, srcPath := splitSessionPath(args[0])
	destSession, destPath := splitSessionPath(args[1])
	switch {
	case srcSession != "" && destSession != "":
		return errors.New("cp: copy between a session and this machine, not between two sessions")
	case destSession != "":
		return copyIn(srcPath, destSession, destPath, *follow)
	case srcSession != "":
		return copyOut(srcSession, srcPath, destPath, *follow)
	}
	return errors.New("cp: one side is SESSION:PATH")
}

// splitSessionPath reads SESSION:PATH. A local path with a colon is written
// so the colon comes after a slash, such as ./a:b.
func splitSessionPath(arg string) (string, string) {
	ref, path, ok := strings.Cut(arg, ":")
	if !ok || ref == "" || strings.Contains(ref, "/") {
		return "", arg
	}
	return ref, path
}

func copyIn(src, ref, dest string, follow bool) error {
	rec, err := session.Find(ref)
	if err != nil {
		return err
	}
	req := session.Request{Op: "cp-in", Path: dest}
	if src == "-" {
		// A tar stream goes into dest, as docker cp - does.
		req.Copy = &session.CopySpec{Base: ".", Dir: true, Contents: true}
	} else {
		spec, err := session.SpecOf(src, follow)
		if err != nil {
			return err
		}
		req.Copy = &spec
	}
	conn, err := session.Dial(rec.ID, req)
	if err != nil {
		return err
	}
	defer conn.Close()
	frames := session.NewFrameWriter(conn)
	sent := make(chan error, 1)
	go func() {
		w := &frameSink{frames}
		var err error
		if src == "-" {
			_, err = io.Copy(w, os.Stdin)
		} else {
			err = session.WriteTar(w, src, *req.Copy)
		}
		frames.Write(session.FrameStdin, nil)
		sent <- err
	}()
	code, err := readOutput(conn)
	if err != nil {
		return err
	}
	if err := <-sent; err != nil {
		return err
	}
	if code != 0 {
		return session.ExitStatus(code)
	}
	return nil
}

// frameSink writes what it is given as standard input frames.
type frameSink struct{ frames *session.FrameWriter }

func (s *frameSink) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += 32 << 10 {
		if err := s.frames.Write(session.FrameStdin, p[off:min(off+32<<10, len(p))]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

func copyOut(ref, src, dest string, follow bool) error {
	rec, err := session.Find(ref)
	if err != nil {
		return err
	}
	contents := strings.HasSuffix(src, "/.")
	conn, err := session.Dial(rec.ID, session.Request{Op: "cp-out", Path: src,
		Copy: &session.CopySpec{Contents: contents, Follow: follow}})
	if err != nil {
		return err
	}
	defer conn.Close()
	var w io.Writer
	if dest == "-" {
		w = os.Stdout
	}
	return session.ReadCopyOut(conn, dest, contents, w)
}

func commandPs(args []string) error {
	fs := flag.NewFlagSet("ps", flag.ContinueOnError)
	quiet := fs.Bool("q", false, "only the IDs")
	fs.BoolVar(quiet, "quiet", false, "only the IDs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	running := session.Running()
	if *quiet {
		for _, rec := range running {
			fmt.Println(rec.ID)
		}
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 3, ' ', 0)
	fmt.Fprintln(w, "SESSION ID\tIMAGE\tCOMMAND\tRUNTIME\tSTARTED\tNAME")
	for _, rec := range running {
		command := strings.Join(rec.Command, " ")
		switch {
		case command == "" && rec.Detached:
			command = "(waits for exec)"
		case command == "":
			command = "(shell)"
		}
		if len(command) > 30 {
			command = command[:27] + "..."
		}
		fmt.Fprintf(w, "%s\t%s\t%q\t%s\t%s ago\t%s\n", rec.ID, rec.Image, command, rec.Runtime, since(rec.Started), rec.Name)
	}
	return w.Flush()
}

// since says how long ago t was, as docker ps does.
func since(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func commandStop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	timeout := fs.Int("t", 10, "seconds to wait after SIGTERM before SIGKILL")
	fs.IntVar(timeout, "time", 10, "seconds to wait after SIGTERM before SIGKILL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("usage: range stop [-t SECONDS] SESSION...")
	}
	var failed error
	for _, ref := range fs.Args() {
		if err := stopSession(ref, *timeout); err != nil {
			fmt.Fprintf(os.Stderr, "range: %v\n", err)
			failed = errors.New("some sessions did not stop")
			continue
		}
		fmt.Println(ref)
	}
	return failed
}

func stopSession(ref string, timeout int) error {
	rec, err := session.Find(ref)
	if err != nil {
		return err
	}
	conn, err := session.Dial(rec.ID, session.Request{Op: "stop", Timeout: timeout})
	if err != nil {
		return err
	}
	defer conn.Close()
	// The connection closes when the session has ended.
	conn.SetReadDeadline(time.Now().Add(time.Duration(timeout)*time.Second + 30*time.Second))
	io.Copy(io.Discard, conn)
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(rec.PID, 0) != nil {
			return nil
		}
	}
	return fmt.Errorf("session %s did not stop", rec.ID)
}

func commandLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "follow the log until the session ends")
	fs.BoolVar(follow, "follow", false, "follow the log until the session ends")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: range logs [-f] SESSION")
	}
	rec, err := session.Find(fs.Arg(0))
	if err != nil {
		// A detached session that has ended still has its log, as a
		// stopped container does.
		path, logErr := session.EndedLog(fs.Arg(0))
		if logErr != nil {
			return err
		}
		data, err := os.ReadFile(path)
		os.Stdout.Write(data)
		return err
	}
	if rec.Log == "" {
		return fmt.Errorf("session %s runs in a terminal: its output is there", rec.ID)
	}
	f, err := os.Open(rec.Log)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		io.Copy(os.Stdout, f)
		if !*follow || syscall.Kill(rec.PID, 0) != nil {
			io.Copy(os.Stdout, f)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
}
