package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/rangetest"
	"github.com/andreygrehov/range/internal/session"
)

func TestCommandsRunEndToEnd(t *testing.T) {
	rangetest.QuietStdout(t)
	data := rangetest.Data(300 << 10)
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(dir, "cache")
	t.Setenv("RANGE_CACHE_DIR", cacheDir)
	t.Setenv("RANGE_BLOCK_SIZE", "64KiB")
	t.Setenv("RANGE_PREFETCH", "off")

	if err := run([]string{"inspect", path}); err != nil {
		t.Fatalf("info: %v", err)
	}
	if err := run([]string{"debug", "read", "--offset", "1000", "--length", "256", path}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := run([]string{"stats"}); err != nil {
		t.Fatalf("stats: %v", err)
	}
	if err := run([]string{"cache", "stats"}); err != nil {
		t.Fatalf("cache stats: %v", err)
	}
	if err := run([]string{"cache", "clear", path}); err != nil {
		t.Fatalf("cache clear: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(cacheDir, "objects"))
	if err != nil {
		t.Fatalf("read cache dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("cache clear left %d artifacts behind", len(entries))
	}

	if err := run([]string{"debug", "read", "--length", "0", path}); err == nil {
		t.Fatal("expected an error for a zero length read")
	}
	if err := run([]string{"inspect"}); err == nil {
		t.Fatal("expected an error when the uri is missing")
	}
	if err := run([]string{"nonsense"}); err == nil {
		t.Fatal("expected an error for an unknown command")
	}
	if err := run([]string{"help"}); err != nil {
		t.Fatalf("help: %v", err)
	}
}

func TestWritableExportIsRefused(t *testing.T) {
	rangetest.QuietStdout(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(path, rangetest.Data(64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RANGE_CACHE_DIR", filepath.Join(dir, "cache"))

	// Asking for a writable export must be an explicit error, not silently
	// downgraded to read-only.
	for _, args := range [][]string{
		{"debug", "nbd", "serve", "--read-only=false", path},
		{"debug", "nbd", "attach", "--read-only=false", path, "/dev/nbd0"},
	} {
		err := run(args)
		if err == nil {
			t.Fatalf("%v: expected an error", args)
		}
		if !strings.Contains(err.Error(), "writable") {
			t.Fatalf("%v: error should explain writes are unsupported, got %v", args, err)
		}
	}
}

func TestReadCommandHonoursBlockSizeFlag(t *testing.T) {
	rangetest.QuietStdout(t)
	data := rangetest.Data(300 << 10)
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RANGE_CACHE_DIR", filepath.Join(dir, "cache"))
	if err := run([]string{"debug", "read", "--block-size=1KiB", "--length", "16", path}); err == nil {
		t.Fatal("expected an error for a block size below the 4KiB minimum")
	}
	if err := run([]string{"debug", "read", "--block-size=8KiB", "--length", "16", path}); err != nil {
		t.Fatalf("read with a valid block size: %v", err)
	}
}

func TestChildRefusesWithoutSession(t *testing.T) {
	if err := run([]string{session.ChildCommand}); err == nil {
		t.Fatal("expected an error when no session directory is given")
	}
	if err := run([]string{session.ChildCommand, t.TempDir()}); err == nil {
		t.Fatal("expected an error when session.json is missing")
	}
}

// The public surface is the environment one; the plumbing stays reachable but
// out of the way.
func TestCommandSurface(t *testing.T) {
	rangetest.QuietStdout(t)
	for _, name := range []string{"build", "publish", "shell", "run", "inspect", "debug"} {
		if !strings.Contains(usage, "range "+name) {
			t.Errorf("usage does not mention %q", name)
		}
	}
	for _, name := range []string{"range nbd ", "range read ", "range cat ", "range site"} {
		if strings.Contains(usage, name) {
			t.Errorf("usage still advertises the plumbing: %q", name)
		}
	}
	if err := run([]string{"debug", "nonsense"}); err == nil {
		t.Error("expected an error for an unknown debug command")
	}
	if err := run([]string{"run", "some-uri"}); err == nil {
		t.Error("run without -- should say how to pass a command")
	}
}

// Go's os.Chmod ignores a literal 0o1000; the sticky bit has to be spelled
// os.ModeSticky, or every environment's /tmp is world-writable without it.
func TestStickyBitNeedsModeSticky(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSticky == 0 {
		t.Fatalf("mode %v has no sticky bit", info.Mode())
	}
}
