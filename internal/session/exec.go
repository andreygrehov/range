package session

import (
	"archive/tar"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ExecCommand runs a request inside a session's namespaces, as range starts
// it there with nsenter: the connection is fd 3.
const ExecCommand = "__exec"

// Executor runs requests in one session, where its namespaces are: natively
// on this machine, or in the guest of Range's VM.
type Executor struct {
	sessionDir string
	noNetwork  bool
	cgroupFD   int

	mu      sync.Mutex
	unshare int // the unshare(1) whose child is the session's first process
}

// NewExecutor runs requests in sess.
func NewExecutor(sess *Session, noNetwork bool) *Executor {
	return &Executor{sessionDir: sess.dir, noNetwork: noNetwork, cgroupFD: sess.cgroup.fd()}
}

// Started records the unshare(1) that runs the session.
func (e *Executor) Started(pid int) error {
	e.mu.Lock()
	e.unshare = pid
	e.mu.Unlock()
	return nil
}

// Handle runs req, whose line was read from conn, in the session, and
// answers on conn.
func (e *Executor) Handle(conn *os.File, line []byte) {
	defer conn.Close()
	frames := NewFrameWriter(conn)
	fail := func(err error) { frames.Write(FrameError, []byte(err.Error())) }
	e.mu.Lock()
	unshare := e.unshare
	e.mu.Unlock()
	first, err := firstProcess(unshare)
	if err != nil {
		fail(err)
		return
	}
	self, err := os.Executable()
	if err != nil {
		fail(err)
		return
	}
	args := []string{"--target", strconv.Itoa(first), "--mount", "--uts", "--ipc", "--pid"}
	if e.noNetwork {
		args = append(args, "--net")
	}
	args = append(args, "--", self, ExecCommand, e.sessionDir, string(line))
	cmd := exec.Command("nsenter", args...)
	cmd.ExtraFiles = []*os.File{conn}
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = intoCgroup(nil, e.cgroupFD)
	if err := cmd.Start(); err != nil {
		fail(fmt.Errorf("enter the session: %w", err))
		return
	}
	conn.Close() // the command has it now
	cmd.Wait()
}

// Stop ends the session: SIGTERM to its first process, then, after timeout,
// SIGKILL, which ends every process in it, as docker stop does.
func (e *Executor) Stop(timeout time.Duration) {
	e.mu.Lock()
	unshare := e.unshare
	e.mu.Unlock()
	first, err := firstProcess(unshare)
	if err != nil {
		return
	}
	syscall.Kill(first, syscall.SIGTERM)
	time.AfterFunc(timeout, func() { syscall.Kill(first, syscall.SIGKILL) })
}

// firstProcess is the first process of the session unshare runs: the one
// whose namespaces, all of them, are the session's.
func firstProcess(unshare int) (int, error) {
	if unshare == 0 {
		return 0, errors.New("the session is still starting")
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", unshare, unshare))
		if err != nil {
			return 0, errors.New("the session has ended")
		}
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			return strconv.Atoi(fields[0])
		}
	}
	return 0, errors.New("the session has no process yet")
}

// ServeControl serves sess's socket for range exec, range cp and range stop:
// exec and cp go to handle, with the connection; stop to stop. It returns
// what stops serving.
func ServeControl(id string, handle func(conn *os.File, req Request, line []byte), stop func(timeout time.Duration)) (func(), error) {
	path := SocketPath(id)
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen for range exec: %w", err)
	}
	os.Chmod(path, 0o600)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				req, line, err := readRequest(c)
				if err != nil {
					c.Close()
					return
				}
				if req.Op == "stop" {
					stop(time.Duration(max(req.Timeout, 0)) * time.Second)
					// The session's end closes the connection.
					io.Copy(io.Discard, c)
					c.Close()
					return
				}
				f, err := c.(*net.UnixConn).File()
				c.Close()
				if err != nil {
					return
				}
				handle(f, req, line)
			}()
		}
	}()
	return func() { l.Close(); os.Remove(path) }, nil
}

// RunExec is ExecCommand: in the session's namespaces, it enters the
// session's root and does what the request asks, answering on fd 3.
func RunExec(args []string) error {
	conn := os.NewFile(3, "conn")
	if conn == nil || len(args) != 2 {
		return errors.New(ExecCommand + " is started by range, not by hand")
	}
	frames := NewFrameWriter(conn)
	fail := func(err error) error {
		frames.Write(FrameError, []byte(err.Error()))
		return nil
	}
	data, err := os.ReadFile(filepath.Join(args[0], "session.json"))
	if err != nil {
		return fail(fmt.Errorf("read the session: %w", err))
	}
	var cfg sessionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fail(err)
	}
	var req Request
	if err := json.Unmarshal([]byte(args[1]), &req); err != nil {
		return fail(err)
	}
	if err := syscall.Chroot(cfg.Root); err != nil {
		return fail(fmt.Errorf("enter the session's root: %w", err))
	}
	workdir := cfg.Workdir
	if req.Workdir != "" {
		workdir = req.Workdir
	}
	if workdir == "" {
		workdir = "/"
	}
	switch req.Op {
	case "exec":
		code, err := execInSession(conn, frames, cfg, req, workdir)
		if err != nil {
			return fail(err)
		}
		frames.Write(FrameExit, exitFrame(code))
	case "cp-in":
		if err := copyIn(conn, req, workdir); err != nil {
			return fail(err)
		}
		frames.Write(FrameExit, exitFrame(0))
	case "cp-out":
		if err := copyOut(frames, req, workdir); err != nil {
			return fail(err)
		}
		frames.Write(FrameExit, exitFrame(0))
	default:
		return fail(fmt.Errorf("unknown request %q", req.Op))
	}
	return nil
}

// inSession resolves a path of a request against the session's workdir.
func inSession(path, workdir string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workdir, path) + map[bool]string{true: "/", false: ""}[strings.HasSuffix(path, "/")]
}

func copyIn(conn *os.File, req Request, workdir string) error {
	if req.Copy == nil {
		return errors.New("a copy names no source")
	}
	plan, err := PlanCopy(*req.Copy, inSession(req.Path, workdir))
	if err != nil {
		return err
	}
	return ExtractTar(&frameReader{r: conn, kind: FrameStdin}, plan, nil)
}

func copyOut(frames *FrameWriter, req Request, workdir string) error {
	src := inSession(req.Path, workdir)
	follow := req.Copy != nil && req.Copy.Follow
	if req.Copy != nil && req.Copy.Contents {
		src = strings.TrimSuffix(src, "/") + "/."
	}
	spec, err := SpecOf(src, follow)
	if err != nil {
		return err
	}
	w := &frameStream{frames: frames, kind: FrameStdout}
	if err := WriteTar(w, src, spec); err != nil {
		return err
	}
	return frames.Write(FrameStdout, nil)
}

// frameStream writes what it is given as frames of one kind.
type frameStream struct {
	frames *FrameWriter
	kind   byte
}

func (s *frameStream) Write(p []byte) (int, error) {
	for off := 0; off < len(p); off += 32 << 10 {
		end := min(off+32<<10, len(p))
		if err := s.frames.Write(s.kind, p[off:end]); err != nil {
			return off, err
		}
	}
	return len(p), nil
}

// execInSession runs req's command and returns its exit status.
func execInSession(conn *os.File, frames *FrameWriter, cfg sessionConfig, req Request, workdir string) (int, error) {
	if len(req.Argv) == 0 {
		return 0, errors.New("no command to run")
	}
	env := mergeEnv(childEnv(cfg), req.Env)
	program, err := lookPathInEnvironment(req.Argv[0], env)
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(program, req.Argv[1:]...)
	cmd.Args[0] = req.Argv[0]
	cmd.Env, cmd.Dir = env, workdir
	if err := os.Chdir(workdir); err != nil {
		return 0, fmt.Errorf("%s: %w", workdir, err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	user := req.User
	if user == nil {
		user = cfg.User
	}
	if user != nil {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(user.UID), Gid: uint32(user.GID), Groups: []uint32{}}
	}

	var outputs sync.WaitGroup
	var input io.Writer
	var terminal *os.File
	if req.TTY {
		master, slave, err := OpenPTY()
		if err != nil {
			return 0, fmt.Errorf("a terminal for the command: %w", err)
		}
		defer master.Close()
		if req.Rows > 0 && req.Cols > 0 {
			SetWindowSize(master, req.Rows, req.Cols)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr.Setsid, cmd.SysProcAttr.Setctty, cmd.SysProcAttr.Ctty = true, true, 0
		if err := cmd.Start(); err != nil {
			slave.Close()
			return 0, err
		}
		slave.Close()
		terminal, input = master, master
		outputs.Add(1)
		go func() { defer outputs.Done(); frames.Stream(FrameStdout, master) }()
	} else {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return 0, err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return 0, err
		}
		var stdin io.WriteCloser
		if req.Interactive {
			if stdin, err = cmd.StdinPipe(); err != nil {
				return 0, err
			}
		}
		if err := cmd.Start(); err != nil {
			return 0, err
		}
		if stdin != nil {
			input = stdin
		}
		outputs.Add(2)
		go func() { defer outputs.Done(); frames.Stream(FrameStdout, stdout) }()
		go func() { defer outputs.Done(); frames.Stream(FrameStderr, stderr) }()
	}

	// Input, the terminal's size and signals come from the client.
	signalTo := cmd.Process.Pid
	if req.TTY {
		signalTo = -cmd.Process.Pid // the terminal's process group
	}
	go func() {
		in := &frameReader{r: conn, kind: FrameStdin, other: func(kind byte, data []byte) {
			switch kind {
			case FrameResize:
				var rows, cols uint16
				if _, err := fmt.Sscanf(string(data), "%d %d", &rows, &cols); err == nil && terminal != nil {
					SetWindowSize(terminal, rows, cols)
				}
			case FrameSignal:
				if sig, err := strconv.Atoi(string(data)); err == nil {
					syscall.Kill(signalTo, syscall.Signal(sig))
				}
			}
		}}
		if input != nil {
			io.Copy(input, in)
			if closer, ok := input.(io.Closer); ok && terminal == nil {
				closer.Close()
			}
		}
		io.Copy(io.Discard, in) // signals and sizes until the client goes
	}()

	if terminal != nil {
		err = cmd.Wait()
		// What the command wrote last is still in the terminal: read it,
		// but not forever, as a process it left behind may hold the terminal.
		waitFor(&outputs, time.Second)
	} else {
		outputs.Wait()
		err = cmd.Wait()
	}
	return statusOf(err)
}

// statusOf is the exit status a command's end gives, as a shell reports it.
func statusOf(err error) (int, error) {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

func waitFor(wg *sync.WaitGroup, limit time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
	}
}

// mergeEnv sets each KEY=VALUE of over in env.
func mergeEnv(env, over []string) []string {
	for _, kv := range over {
		key, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, have := range env {
			if strings.HasPrefix(have, key+"=") {
				env[i], replaced = kv, true
			}
		}
		if !replaced {
			env = append(env, kv)
		}
	}
	return env
}

// ReadCopyOut reads a copy out of a session, from its frames, into dest on
// this machine, or as a tar stream to w when w is not nil.
func ReadCopyOut(conn io.Reader, dest string, contents bool, w io.Writer) error {
	var failure error
	var status = -1
	in := &frameReader{r: conn, kind: FrameStdout, other: func(kind byte, data []byte) {
		switch kind {
		case FrameError:
			failure = errors.New(string(data))
		case FrameExit:
			status, _ = strconv.Atoi(string(data))
		}
	}}
	var err error
	if w != nil {
		_, err = io.Copy(w, in)
	} else {
		err = ExtractTar(in, CopyPlan{}, func(first *tar.Header) (CopyPlan, error) {
			// SRC/. sends what is in it; SRC sends itself first.
			if contents {
				return PlanCopy(CopySpec{Base: ".", Dir: true, Contents: true}, dest)
			}
			top, _, _ := strings.Cut(strings.TrimPrefix(first.Name, "./"), "/")
			return PlanCopy(CopySpec{Base: top, Dir: first.Typeflag == tar.TypeDir}, dest)
		})
	}
	if failure == nil && err == nil {
		// The exit frame follows the end of the stream.
		for status < 0 && failure == nil {
			kind, data, rerr := ReadFrame(conn)
			if rerr != nil {
				break
			}
			switch kind {
			case FrameError:
				failure = errors.New(string(data))
			case FrameExit:
				status, _ = strconv.Atoi(string(data))
			}
		}
	}
	if failure != nil {
		return failure
	}
	return err
}
