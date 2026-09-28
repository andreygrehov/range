package core

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

// TraceWriter appends one JSON line per remote fetch, for --trace.
type TraceWriter struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
	start  time.Time
}

// NewTraceWriter creates or truncates the trace file at path.
func NewTraceWriter(path string) (*TraceWriter, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &TraceWriter{file: file, writer: bufio.NewWriter(file), start: time.Now()}, nil
}

type traceRecord struct {
	MillisSinceStart int64  `json:"ms"`
	FirstBlock       int64  `json:"first_block"`
	LastBlock        int64  `json:"last_block"`
	Offset           int64  `json:"offset"`
	Length           int64  `json:"length"`
	Source           string `json:"source"`
	LatencyMillis    int64  `json:"latency_ms"`
}

func (t *TraceWriter) record(rec traceRecord) {
	if t == nil {
		return
	}
	rec.MillisSinceStart = time.Since(t.start).Milliseconds()
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writer.Write(line)
	t.writer.WriteByte('\n')
}

// Close flushes and closes the trace file.
func (t *TraceWriter) Close() error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.writer.Flush()
	return t.file.Close()
}
