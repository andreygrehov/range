package session

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A session can be reached while it runs, as a container can: range exec
// runs a command in it, range cp copies files in and out, range stop ends
// it. Each session listens on a Unix socket in the registry, a directory
// private to the user, next to a file that describes it for range ps. The
// process that runs the session serves the socket: range itself natively,
// the process that runs the VM otherwise, which passes each request on to
// the guest over vsock. There is no daemon: a session's socket lives and
// dies with it.

// Record is what the registry holds of a running session.
type Record struct {
	ID       string    `json:"id"`
	Name     string    `json:"name,omitempty"`
	Image    string    `json:"image"`
	Command  []string  `json:"command,omitempty"`
	Runtime  string    `json:"runtime"`
	PID      int       `json:"pid"` // the range that runs it
	Started  time.Time `json:"started"`
	Detached bool      `json:"detached,omitempty"`
	Log      string    `json:"log,omitempty"`
}

// RegistryDir is where running sessions are listed: under XDG_RUNTIME_DIR,
// or a directory of the user's own in the temporary directory. Socket paths
// are short there, as they must be.
func RegistryDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		return filepath.Join(dir, "range")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("range-%d", os.Geteuid()))
}

// SocketPath is where session id listens.
func SocketPath(id string) string { return filepath.Join(RegistryDir(), id+".sock") }

// LogPath is where a detached session's output goes.
func LogPath(id string) string { return filepath.Join(RegistryDir(), id+".log") }

// Register lists rec, and returns what takes it off the list.
func Register(rec Record) (func(), error) {
	dir := RegistryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// A directory someone else made, or left open, is not ours to trust.
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 || !ownedByMe(info) {
		return nil, fmt.Errorf("%s must be a directory of yours that only you can open", dir)
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, rec.ID+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, err
	}
	pruneLogs(dir)
	return func() { os.Remove(path); os.Remove(SocketPath(rec.ID)) }, nil
}

func ownedByMe(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Geteuid()
}

// Running lists the sessions that run, oldest first, and forgets those
// whose range is gone.
func Running() []Record {
	paths, _ := filepath.Glob(filepath.Join(RegistryDir(), "*.json"))
	var out []Record
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var rec Record
		if json.Unmarshal(data, &rec) != nil || rec.ID == "" {
			continue
		}
		if syscall.Kill(rec.PID, 0) != nil && !errors.Is(syscall.Kill(rec.PID, 0), syscall.EPERM) {
			os.Remove(path)
			os.Remove(SocketPath(rec.ID))
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Find is the running session ref names: its ID, a prefix of it no other
// shares, or its name.
func Find(ref string) (Record, error) {
	var matches []Record
	for _, rec := range Running() {
		switch {
		case rec.ID == ref || rec.Name != "" && rec.Name == ref:
			return rec, nil
		case strings.HasPrefix(rec.ID, ref):
			matches = append(matches, rec)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Record{}, fmt.Errorf("no running session %q: range ps lists them", ref)
	}
	return Record{}, fmt.Errorf("%q names more than one session: give more of its ID", ref)
}

// EndedLog is the log of a detached session that has ended: ref is its ID,
// or a prefix of it no other log shares.
func EndedLog(ref string) (string, error) {
	paths, _ := filepath.Glob(filepath.Join(RegistryDir(), "*.log"))
	var matches []string
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".log")
		if id == ref {
			return p, nil
		}
		if strings.HasPrefix(id, ref) {
			matches = append(matches, p)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("no log of a session %q", ref)
	}
	return matches[0], nil
}

// pruneLogs removes the logs of sessions that ended a week ago or more.
func pruneLogs(dir string) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	for _, p := range paths {
		id := strings.TrimSuffix(filepath.Base(p), ".log")
		if _, err := os.Stat(filepath.Join(dir, id+".json")); err == nil {
			continue
		}
		if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) > 7*24*time.Hour {
			os.Remove(p)
		}
	}
}

// Request is the first line on a session's socket.
type Request struct {
	Op string `json:"op"` // exec, cp-in, cp-out or stop

	// exec
	Argv        []string `json:"argv,omitempty"`
	Env         []string `json:"env,omitempty"`
	Workdir     string   `json:"workdir,omitempty"`
	User        *User    `json:"user,omitempty"`
	TTY         bool     `json:"tty,omitempty"`
	Interactive bool     `json:"interactive,omitempty"`
	Rows        uint16   `json:"rows,omitempty"`
	Cols        uint16   `json:"cols,omitempty"`

	// cp-in and cp-out
	Path string    `json:"path,omitempty"`
	Copy *CopySpec `json:"copy,omitempty"`

	// stop: seconds to wait after SIGTERM before SIGKILL
	Timeout int `json:"timeout,omitempty"`
}

// Frames follow the request, both ways: a kind, a big-endian length, and
// that many bytes.
const (
	FrameStdin  = 'i' // to the session; empty is the end of input
	FrameResize = 'r' // to the session: "ROWS COLS"
	FrameSignal = 's' // to the session: a signal number
	FrameStdout = 'o' // from the session
	FrameStderr = 'e' // from the session
	FrameExit   = 'x' // from the session, last: the exit status
	FrameError  = 'E' // from the session, last: why it could not run
)

const maxFrame = 1 << 20

// FrameWriter writes frames from any goroutine.
type FrameWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// NewFrameWriter writes frames to w.
func NewFrameWriter(w io.Writer) *FrameWriter { return &FrameWriter{w: w} }

// Write sends one frame.
func (f *FrameWriter) Write(kind byte, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	var head [5]byte
	head[0] = kind
	binary.BigEndian.PutUint32(head[1:], uint32(len(data)))
	if _, err := f.w.Write(head[:]); err != nil {
		return err
	}
	_, err := f.w.Write(data)
	return err
}

// Stream sends r as frames of kind, and nothing when r ends.
func (f *FrameWriter) Stream(kind byte, r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if werr := f.Write(kind, buf[:n]); werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var head [5]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(head[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("a frame of %d bytes", n)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return head[0], data, nil
}

// frameReader reads the data of frames of one kind as a stream, until the
// empty frame of that kind; it hands frames of other kinds to other.
type frameReader struct {
	r     io.Reader
	kind  byte
	other func(kind byte, data []byte)
	left  []byte
	done  bool
}

func (f *frameReader) Read(p []byte) (int, error) {
	for len(f.left) == 0 {
		if f.done {
			return 0, io.EOF
		}
		kind, data, err := ReadFrame(f.r)
		if err != nil {
			return 0, err
		}
		if kind != f.kind {
			if f.other != nil {
				f.other(kind, data)
			}
			continue
		}
		if len(data) == 0 {
			f.done = true
			return 0, io.EOF
		}
		f.left = data
	}
	n := copy(p, f.left)
	f.left = f.left[n:]
	return n, nil
}

// Dial connects to session id and sends req.
func Dial(id string, req Request) (net.Conn, error) {
	conn, err := net.Dial("unix", SocketPath(id))
	if err != nil {
		return nil, fmt.Errorf("session %s does not answer: %w", id, err)
	}
	line, _ := json.Marshal(req)
	if _, err := conn.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// ReadRequest reads the request line of a connection to a session.
func ReadRequest(r io.Reader) (Request, []byte, error) { return readRequest(r) }

// readRequest reads the request line, a byte at a time, so nothing after it
// is taken from the connection.
func readRequest(r io.Reader) (Request, []byte, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < 1<<20 {
		if _, err := io.ReadFull(r, b); err != nil {
			return Request{}, nil, err
		}
		if b[0] == '\n' {
			var req Request
			err := json.Unmarshal(line, &req)
			return req, line, err
		}
		line = append(line, b[0])
	}
	return Request{}, nil, errors.New("a request line too long")
}

// exitFrame is the frame that ends a request with status code.
func exitFrame(code int) []byte { return []byte(strconv.Itoa(code)) }
