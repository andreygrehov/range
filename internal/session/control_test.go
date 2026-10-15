package session

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

func TestFrames(t *testing.T) {
	var buf bytes.Buffer
	w := NewFrameWriter(&buf)
	w.Write(FrameResize, []byte("24 80"))
	w.Stream(FrameStdin, bytes.NewReader([]byte("hello, session")))
	w.Write(FrameStdin, nil)
	w.Write(FrameExit, exitFrame(3))

	var resized string
	in := &frameReader{r: &buf, kind: FrameStdin, other: func(kind byte, data []byte) {
		if kind == FrameResize {
			resized = string(data)
		}
	}}
	got, err := io.ReadAll(in)
	if err != nil || string(got) != "hello, session" || resized != "24 80" {
		t.Fatalf("read %q, %v; resized %q", got, err, resized)
	}
	if kind, data, err := ReadFrame(&buf); err != nil || kind != FrameExit || string(data) != "3" {
		t.Fatalf("exit frame: %c %q %v", kind, data, err)
	}
}

func TestFindSessions(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	for _, rec := range []Record{
		{ID: "a1b2c3", Image: "ubuntu", PID: os.Getpid(), Started: time.Now()},
		{ID: "a1ffff", Image: "python", Name: "agent", PID: os.Getpid(), Started: time.Now()},
		{ID: "dead00", Image: "gone", PID: 1 << 30, Started: time.Now()}, // its range is gone
	} {
		if _, err := Register(rec); err != nil {
			t.Fatal(err)
		}
	}
	for ref, want := range map[string]string{"a1b2c3": "a1b2c3", "a1b": "a1b2c3", "agent": "a1ffff", "a1f": "a1ffff"} {
		if rec, err := Find(ref); err != nil || rec.ID != want {
			t.Errorf("%s: %s, %v", ref, rec.ID, err)
		}
	}
	for _, ref := range []string{"a1", "dead00", "zz"} {
		if rec, err := Find(ref); err == nil {
			t.Errorf("%s found %s", ref, rec.ID)
		}
	}
	if n := len(Running()); n != 2 {
		t.Errorf("%d running, want 2", n)
	}
}

// sudo -E keeps the XDG_RUNTIME_DIR of the user who ran it: root does not
// list its sessions there.
func TestRegistryIsMine(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/")
	if os.Geteuid() != 0 && RegistryDir() == "/range" {
		t.Error("the registry went into a directory that is not this user's")
	}
}
