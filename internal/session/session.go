package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/andreygrehov/range/internal/environment"
)

// sessionConfig is what the parent hands to the re-executed child through
// session.json. By the time the child reads it, it is already inside the new
// namespaces.
type sessionConfig struct {
	Root        string            `json:"root"`
	Shell       string            `json:"shell"`
	Workdir     string            `json:"workdir"`
	Hostname    string            `json:"hostname"`
	Environment map[string]string `json:"environment"`
	Command     []string          `json:"command,omitempty"`
	NVIDIA      *NVIDIA           `json:"nvidia,omitempty"`
}

// State is where a session is in its life, as shown by range stats.
type State string

const (
	StateCreating  State = "CREATING"
	StateAttaching State = "ATTACHING"
	StateMounting  State = "MOUNTING"
	StateReady     State = "READY"
	StateRunning   State = "RUNNING"
	StateStopping  State = "STOPPING"
	StateStopped   State = "STOPPED"
	StateFailed    State = "FAILED"
)

// Session is one environment from attach to cleanup: its state, the
// directories it mounted and the steps that undo them.
type Session struct {
	ID        string
	Name      string
	State     State
	StartedAt time.Time
	ReadyIn   time.Duration

	dir        string
	lower      string
	Upper      string
	work       string
	root       string
	device     string
	Persistent bool
	Meta       environment.Metadata

	cleanups  []func() error
	cleanOnce sync.Once
}

// NewID returns a short random session identifier.
func NewID() string {
	raw := make([]byte, 3)
	if _, err := rand.Read(raw); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()%0xffffff, 16)
	}
	return hex.EncodeToString(raw)
}

// Push registers an undo step. Steps run in reverse order during cleanup.
func (s *Session) Push(step func() error) { s.cleanups = append(s.cleanups, step) }

// Cleanup unwinds the session. It runs once, in reverse order, and continues
// past failures so that one stuck unmount cannot strand an nbd device.
func (s *Session) Cleanup() error {
	var firstErr error
	s.cleanOnce.Do(func() {
		s.State = StateStopping
		for i := len(s.cleanups) - 1; i >= 0; i-- {
			if err := s.cleanups[i](); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		s.cleanups = nil
		s.State = StateStopped
	})
	return firstErr
}

// writableLayer decides where the environment's writes land: an explicit
// directory, a named environment that survives across runs, or a throwaway
// directory inside the session. Anything meant to outlive the shell must sit
// outside the session directory, which is deleted on the way out.
func writableLayer(sess *Session, upperDir, name string, keep bool) (upper, work string, persistent bool, err error) {
	named := func(key string) (string, string, bool, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", "", false, err
		}
		base := filepath.Join(home, ".local", "share", "range", "environments", key)
		return filepath.Join(base, "upper"), filepath.Join(base, "work"), true, nil
	}
	switch {
	case upperDir != "":
		return filepath.Join(upperDir, "upper"), filepath.Join(upperDir, "work"), true, nil
	case name != "":
		return named(name)
	case keep:
		return named(sess.ID)
	default:
		return filepath.Join(sess.dir, "upper"), filepath.Join(sess.dir, "work"), false, nil
	}
}

// Prepare lays out the session directories and registers their removal.
// It runs wherever the kernel work happens: this host on Linux, the guest
// otherwise.
func Prepare(sess *Session, upperDir, envName string, keep bool) error {
	sess.dir = filepath.Join("/run/range", sess.ID)
	sess.lower = filepath.Join(sess.dir, "lower")
	sess.root = filepath.Join(sess.dir, "root")
	upper, work, persistent, err := writableLayer(sess, upperDir, envName, keep)
	if err != nil {
		return err
	}
	sess.Upper, sess.work, sess.Persistent = upper, work, persistent
	for _, dir := range []string{sess.lower, sess.root, sess.Upper, sess.work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	sess.Push(func() error { return os.RemoveAll(sess.dir) })
	return nil
}
