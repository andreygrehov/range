// Package coretest opens real core.Readers for the tests of packages built on
// top of core. core's own tests keep private copies of these helpers, since a
// package's internal tests cannot import a package that imports it.
package coretest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/rangetest"
)

// Config returns a small, fast configuration with its cache in a temp dir.
func Config(t *testing.T, tune func(*core.Config)) core.Config {
	t.Helper()
	c := core.DefaultConfig()
	c.CacheDir = t.TempDir()
	c.BlockSize = 64 << 10
	c.MaxRangeSize = 8 << 20
	c.MemoryCache = 4 << 20
	c.DiskCache = 64 << 20
	c.Prefetch = false
	if tune != nil {
		tune(&c)
	}
	if c.MaxRangeSize < c.BlockSize {
		c.MaxRangeSize = c.BlockSize
	}
	return c
}

// NewReader writes data to a temp file, opens it, and swaps in a counting
// backend so tests can see exactly what crossed the "network".
func NewReader(t *testing.T, data []byte, tune func(*core.Config)) (*core.Reader, *rangetest.CountingBackend) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "artifact.bin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	r, err := core.Open(context.Background(), path, Config(t, tune))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { r.Close() })
	counter := &rangetest.CountingBackend{Inner: r.Backend}
	r.Backend = counter
	return r, counter
}
