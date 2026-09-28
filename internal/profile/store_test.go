package profile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/object"
)

func TestProfileTimingSurvivesSerialization(t *testing.T) {
	dir := t.TempDir()
	ident := testIdentity()
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	p := merge(Profile{}, ident, 1<<20, "go-test", observe(map[int64]int{4: 30, 5: 31, 12: 900}), now)
	path := Path(dir, ident, "go-test")
	if err := WriteFile(path, p); err != nil {
		t.Fatalf("write: %v", err)
	}
	loaded, err := ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(loaded.Ranges) != len(p.Ranges) {
		t.Fatalf("ranges = %d, want %d", len(loaded.Ranges), len(p.Ranges))
	}
	for i := range p.Ranges {
		if loaded.Ranges[i] != p.Ranges[i] {
			t.Fatalf("range %d changed across serialization:\n got %+v\nwant %+v", i, loaded.Ranges[i], p.Ranges[i])
		}
	}
	if loaded.Workload != "go-test" || loaded.Sessions != 1 {
		t.Fatalf("header lost: workload=%q sessions=%d", loaded.Workload, loaded.Sessions)
	}
}

func TestProfileIsolationByWorkloadVersionAndBlockSize(t *testing.T) {
	dir := t.TempDir()
	ident := testIdentity()
	now := time.Now()

	if _, err := Save(dir, ident, 1<<20, "go-test", observe(map[int64]int{1: 10}), now); err != nil {
		t.Fatal(err)
	}
	if _, err := Save(dir, ident, 1<<20, "npm-ci", observe(map[int64]int{500: 10, 501: 11}), now); err != nil {
		t.Fatal(err)
	}

	t.Run("workloads do not collide", func(t *testing.T) {
		goTest, ok := Load(dir, ident, 1<<20, "go-test")
		if !ok || goTest.BlockTotal() != 1 {
			t.Fatalf("go-test profile = %+v, ok=%v", goTest.Ranges, ok)
		}
		npm, ok := Load(dir, ident, 1<<20, "npm-ci")
		if !ok || npm.BlockTotal() != 2 {
			t.Fatalf("npm-ci profile = %+v, ok=%v", npm.Ranges, ok)
		}
	})

	t.Run("block size does not collide", func(t *testing.T) {
		if _, ok := Load(dir, ident, 4<<20, "go-test"); ok {
			t.Fatal("a profile recorded at another block size must not be reused")
		}
	})

	t.Run("artifact version does not collide", func(t *testing.T) {
		changed := ident
		changed.ETag = "etag-2"
		if _, ok := Load(dir, changed, 1<<20, "go-test"); ok {
			t.Fatal("a profile for another generation of the artifact must not be reused")
		}
	})
}

func TestProfileVersionCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "p.json")

	t.Run("unknown additive fields are tolerated", func(t *testing.T) {
		body := `{"version":1,"artifact":{"uri":"s3://b/k","size":100,"etag":"e"},
			"blockSize":1048576,"workload":"go-test","sessions":3,
			"ranges":[{"startBlock":1,"count":2,"observations":3,"firstSeenMs":5,
			           "meanFirstUseMs":7,"lastSeen":"2026-01-01T00:00:00Z","futureField":42}],
			"somethingNew":{"a":1}}`
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		p, err := ReadFile(path)
		if err != nil {
			t.Fatalf("a v1 reader must tolerate unknown fields: %v", err)
		}
		if p.Sessions != 3 || p.BlockTotal() != 2 {
			t.Fatalf("parsed profile = %+v", p)
		}
	})

	t.Run("unsupported major versions are rejected", func(t *testing.T) {
		if err := os.WriteFile(path, []byte(`{"version":2,"ranges":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFile(path); !errors.Is(err, errVersion) {
			t.Fatalf("err = %v, want errProfileVersion", err)
		}
	})
}

func TestProfileWriteIsAtomic(t *testing.T) {
	dir := t.TempDir()
	ident := testIdentity()
	path := Path(dir, ident, "go-test")
	good := merge(Profile{}, ident, 1<<20, "go-test", observe(map[int64]int{1: 1}), time.Now())
	if err := WriteFile(path, good); err != nil {
		t.Fatal(err)
	}
	// Whatever else is in the directory, exactly one profile file must be
	// visible and it must parse.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".profile-") {
			t.Fatalf("temporary file %q left behind", entry.Name())
		}
	}
	if _, err := ReadFile(path); err != nil {
		t.Fatalf("stored profile does not parse: %v", err)
	}
}

func TestProfileLockSerialisesWriters(t *testing.T) {
	dir := t.TempDir()
	ident := testIdentity()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if _, err := Save(dir, ident, 1<<20, "go-test",
				observe(map[int64]int{int64(n): n}), time.Now()); err != nil {
				t.Errorf("save %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
	p, ok := Load(dir, ident, 1<<20, "go-test")
	if !ok {
		t.Fatal("profile missing after concurrent writers")
	}
	if p.Sessions != 8 {
		t.Fatalf("Sessions = %d, want 8: a concurrent write was lost", p.Sessions)
	}
	if p.BlockTotal() != 8 {
		t.Fatalf("blockTotal = %d, want 8", p.BlockTotal())
	}
}

func TestProfileExportImportRoundTrip(t *testing.T) {
	// Machine A learns, machine B imports: the file must stand alone.
	machineA, machineB := t.TempDir(), t.TempDir()
	ident := testIdentity()
	if _, err := Save(machineA, ident, 1<<20, "go-test",
		observe(map[int64]int{3: 10, 4: 11, 90: 400}), time.Now()); err != nil {
		t.Fatal(err)
	}
	exported, err := os.ReadFile(Path(machineA, ident, "go-test"))
	if err != nil {
		t.Fatal(err)
	}
	transfer := filepath.Join(t.TempDir(), "profile.json")
	if err := os.WriteFile(transfer, exported, 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := ReadFile(transfer)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := object.Identity{URI: p.Artifact.URI, Size: p.Artifact.Size, ETag: p.Artifact.ETag,
		VersionID: p.Artifact.VersionID, BlockSize: p.BlockSize}
	if err := WriteFile(Path(machineB, rebuilt, p.Workload), p); err != nil {
		t.Fatal(err)
	}
	loaded, ok := Load(machineB, ident, 1<<20, "go-test")
	if !ok {
		t.Fatal("imported profile is not usable on the second machine")
	}
	if loaded.BlockTotal() != 3 || loaded.Sessions != 1 {
		t.Fatalf("imported profile = %+v", loaded)
	}
	if strings.Contains(string(exported), machineA) {
		t.Fatal("the profile embeds a machine-specific path, so it is not portable")
	}
}
