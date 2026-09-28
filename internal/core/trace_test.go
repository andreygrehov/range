package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/rangetest"
)

func TestTraceRecordsRemoteFetches(t *testing.T) {
	data := rangetest.Data(256 << 10)
	r, _ := newTestReader(t, data, func(c *Config) { c.BlockSize = 64 << 10 })
	tracePath := filepath.Join(t.TempDir(), "access.jsonl")
	trace, err := NewTraceWriter(tracePath)
	if err != nil {
		t.Fatalf("newTraceWriter: %v", err)
	}
	r.Trace = trace

	if _, err := r.ReadAt(make([]byte, 4096), 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := trace.Close(); err != nil {
		t.Fatalf("close trace: %v", err)
	}
	r.Trace = nil

	content, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	// v0.2 classifies every fetch, so the trace names the read class.
	if !strings.Contains(string(content), `"source":"demand"`) {
		t.Fatalf("trace did not record a demand fetch: %q", content)
	}
}
