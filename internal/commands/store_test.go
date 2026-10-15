package commands

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testMap() Map {
	return Map{
		"pdflatex": {{Repo: "texlive/texlive", Platforms: []string{"linux/amd64", "linux/arm64"}}},
		"python3":  {{Repo: "python", Platforms: []string{"linux/amd64"}}, {Repo: "haxe"}},
		"nvcc":     {{Repo: "nvidia/cuda", Platforms: []string{"linux/amd64"}}},
	}
}

// serve publishes dir as a catalog, and counts the requests it answers.
func serve(t *testing.T, dir string) (*httptest.Server, *atomic.Int64) {
	var n atomic.Int64
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	t.Cleanup(s.Close)
	return s, &n
}

func TestWriteIsStable(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ra, err := Write(a, testMap(), 2, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := Write(b, testMap(), 2, time.Unix(100, 0))
	for p, d := range ra.Shards {
		if rb.Shards[p] != d {
			t.Errorf("shard %s: %s, then %s: the same map must make the same shards", p, d, rb.Shards[p])
		}
		if _, err := os.Stat(filepath.Join(a, shardPath(d))); err != nil {
			t.Errorf("shard %s: %v", p, err)
		}
	}
}

func TestLookup(t *testing.T) {
	catalog := t.TempDir()
	if _, err := Write(catalog, testMap(), 2, time.Now()); err != nil {
		t.Fatal(err)
	}
	s, requests := serve(t, catalog)
	now := time.Now()
	c := &Client{Base: s.URL, Dir: t.TempDir(), MaxAge: 24 * time.Hour, HTTP: s.Client(), Now: func() time.Time { return now }}
	ctx := context.Background()

	owners, err := c.Lookup(ctx, "pdflatex")
	if err != nil || len(owners) != 1 || owners[0].Repo != "texlive/texlive" {
		t.Fatalf("pdflatex: %v, %v", owners, err)
	}
	if _, err := c.Lookup(ctx, "gti"); !errors.Is(err, ErrUnknown) {
		t.Errorf("an unknown command: %v", err)
	}
	// Both are answered from what the client kept: no request at all.
	before := requests.Load()
	if _, err := c.Lookup(ctx, "pdflatex"); err != nil || requests.Load() != before {
		t.Errorf("a second lookup made %d requests, %v", requests.Load()-before, err)
	}

	// A day later the catalog is gone: the kept map still answers.
	now = now.Add(48 * time.Hour)
	s.Close()
	if owners, err := c.Lookup(ctx, "pdflatex"); err != nil || owners[0].Repo != "texlive/texlive" {
		t.Errorf("offline: %v, %v", owners, err)
	}
}

func TestNames(t *testing.T) {
	catalog := t.TempDir()
	Write(catalog, testMap(), 2, time.Now())
	s, _ := serve(t, catalog)
	c := &Client{Base: s.URL, Dir: t.TempDir(), MaxAge: time.Hour, HTTP: s.Client()}
	names, err := c.Names(context.Background(), nil)
	if err != nil || strings.Join(names, " ") != "nvcc pdflatex python3" {
		t.Errorf("names = %v, %v", names, err)
	}
}

func TestLookupRefusesATamperedShard(t *testing.T) {
	catalog := t.TempDir()
	root, _ := Write(catalog, testMap(), 2, time.Now())
	path := filepath.Join(catalog, shardPath(root.Shards[prefixOf("nvcc", 2)]))
	os.WriteFile(path, []byte("not the shard"), 0o644)
	s, _ := serve(t, catalog)
	c := &Client{Base: s.URL, Dir: t.TempDir(), MaxAge: time.Hour, HTTP: s.Client()}
	if _, err := c.Lookup(context.Background(), "nvcc"); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Errorf("a shard that does not match its digest: %v", err)
	}
}

func TestLookupRefusesANewerFormat(t *testing.T) {
	catalog := t.TempDir()
	os.MkdirAll(filepath.Join(catalog, "commands"), 0o755)
	os.WriteFile(filepath.Join(catalog, RootName), []byte(`{"format": 99, "digits": 2, "shards": {}}`), 0o644)
	s, _ := serve(t, catalog)
	c := &Client{Base: s.URL, Dir: t.TempDir(), MaxAge: time.Hour, HTTP: s.Client()}
	if _, err := c.Lookup(context.Background(), "nvcc"); err == nil || !strings.Contains(err.Error(), "update Range") {
		t.Errorf("a root of a newer format: %v", err)
	}
}
