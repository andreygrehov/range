package session

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionCleanupUnwindsInReverseAndOnlyOnce(t *testing.T) {
	sess := &Session{State: StateRunning}
	var order []string
	for _, step := range []string{"first", "second", "third"} {
		name := step
		sess.Push(func() error {
			order = append(order, name)
			return nil
		})
	}
	if err := sess.Cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if fmt.Sprint(order) != fmt.Sprint([]string{"third", "second", "first"}) {
		t.Fatalf("cleanup order = %v, want reverse registration order", order)
	}
	if sess.State != StateStopped {
		t.Fatalf("state = %s, want %s", sess.State, StateStopped)
	}
	// Idempotent: a second call must not rerun anything.
	if err := sess.Cleanup(); err != nil {
		t.Fatalf("second cleanup: %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("cleanup ran twice: %v", order)
	}
}

func TestSessionCleanupContinuesPastFailure(t *testing.T) {
	sess := &Session{}
	var ran []string
	sess.Push(func() error { ran = append(ran, "disconnect"); return nil })
	sess.Push(func() error { ran = append(ran, "unmount-lower"); return errors.New("device busy") })
	sess.Push(func() error { ran = append(ran, "unmount-overlay"); return nil })

	err := sess.Cleanup()
	if err == nil || !strings.Contains(err.Error(), "device busy") {
		t.Fatalf("cleanup error = %v, want the first failure reported", err)
	}
	// A stuck unmount must not strand the nbd device.
	if fmt.Sprint(ran) != fmt.Sprint([]string{"unmount-overlay", "unmount-lower", "disconnect"}) {
		t.Fatalf("cleanup stopped early: %v", ran)
	}
}

func TestWritableLayer(t *testing.T) {
	sess := &Session{dir: "/run/range/abc123"}

	t.Run("ephemeral by default", func(t *testing.T) {
		upper, work, persistent, err := writableLayer(sess, "", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if persistent {
			t.Fatal("the default writable layer must be ephemeral")
		}
		if upper != "/run/range/abc123/upper" || work != "/run/range/abc123/work" {
			t.Fatalf("upper=%s work=%s", upper, work)
		}
	})

	t.Run("explicit directory persists", func(t *testing.T) {
		upper, work, persistent, err := writableLayer(sess, "/data/env", "", false)
		if err != nil {
			t.Fatal(err)
		}
		if !persistent || upper != "/data/env/upper" || work != "/data/env/work" {
			t.Fatalf("upper=%s work=%s persistent=%v", upper, work, persistent)
		}
	})

	t.Run("keep moves the layer out of the session directory", func(t *testing.T) {
		upper, _, persistent, err := writableLayer(sess, "", "", true)
		if err != nil {
			t.Fatal(err)
		}
		if !persistent {
			t.Fatal("--keep must produce a persistent layer")
		}
		if strings.HasPrefix(upper, sess.dir) {
			t.Fatalf("kept layer %s sits inside the session directory, which is deleted on exit", upper)
		}
	})

	t.Run("named environment persists under the user's data dir", func(t *testing.T) {
		upper, _, persistent, err := writableLayer(sess, "", "acme", false)
		if err != nil {
			t.Fatal(err)
		}
		if !persistent || !strings.HasSuffix(upper, filepath.Join("range", "environments", "acme", "upper")) {
			t.Fatalf("upper = %s persistent = %v", upper, persistent)
		}
	})
}
