package artifact

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// PackStats describes what Pack wrote.
type PackStats struct {
	Logical, Stored       int64
	Chunks, Zero, Deduped int64
	IndexBytes            int64
	Elapsed               time.Duration
}

// Pack turns a raw disk image into a Range artifact. Chunks of all
// zeroes are dropped, identical chunks are stored once, and the rest are
// compressed independently so any one of them can be fetched on its own.
func Pack(source, target string, chunkSize int64) (PackStats, error) {
	var st PackStats
	started := time.Now()
	in, err := os.Open(source)
	if err != nil {
		return st, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return st, err
	}
	if chunkSize < minChunk || chunkSize > maxChunk {
		return st, fmt.Errorf("chunk size must be between 4KiB and 64MiB, got %d", chunkSize)
	}
	if (info.Size()+chunkSize-1)/chunkSize > maxChunks {
		return st, fmt.Errorf("%s needs more than %d chunks of %d bytes; use a larger --chunk-size",
			source, maxChunks, chunkSize)
	}
	out, err := os.Create(target)
	if err != nil {
		return st, err
	}
	defer out.Close()
	if _, err := out.Write(make([]byte, headerSize)); err != nil {
		return st, err
	}

	st.Logical = info.Size()
	st.Chunks = (info.Size() + chunkSize - 1) / chunkSize
	entries := make([]Entry, st.Chunks)
	seen := make(map[[32]byte]Entry, 1024)
	cursor := int64(headerSize)

	// Compression runs on a small pool; the read is sequential so the source
	// is touched once.
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	type job struct {
		index int64
		data  []byte
	}
	type done struct {
		index  int64
		hash   [32]byte
		packed []byte
		flags  uint32
		zero   bool
	}
	jobs := make(chan job, workers*2)
	results := make(chan done, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault),
				zstd.WithEncoderConcurrency(1))
			if err != nil {
				return
			}
			defer enc.Close()
			for j := range jobs {
				d := done{index: j.index, hash: sha256.Sum256(j.data)}
				if allZero(j.data) {
					d.zero = true
					results <- d
					continue
				}
				d.packed = enc.EncodeAll(j.data, nil)
				if len(d.packed) >= len(j.data) {
					d.packed = append([]byte(nil), j.data...)
					d.flags = flagRaw
				}
				results <- d
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	go func() {
		defer close(jobs)
		for index := int64(0); index < st.Chunks; index++ {
			length := chunkSize
			if remaining := info.Size() - index*chunkSize; remaining < length {
				length = remaining
			}
			buf := make([]byte, length)
			if _, err := in.ReadAt(buf, index*chunkSize); err != nil && err != io.EOF {
				return
			}
			jobs <- job{index: index, data: buf}
		}
	}()

	pending := make(map[int64]done, workers*4)
	next := int64(0)
	write := func(d done) error {
		if d.zero {
			st.Zero++
			return nil
		}
		if prior, ok := seen[d.hash]; ok {
			entries[d.index] = prior
			st.Deduped++
			return nil
		}
		if _, err := out.Write(d.packed); err != nil {
			return err
		}
		entry := Entry{Offset: cursor, Length: int32(len(d.packed)), Flags: d.flags, Hash: d.hash}
		entries[d.index] = entry
		seen[d.hash] = entry
		cursor += int64(len(d.packed))
		return nil
	}
	// Results arrive out of order; chunks are written in logical order so that
	// a sequential read is also sequential in the object.
	for d := range results {
		pending[d.index] = d
		for {
			ready, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			if err := write(ready); err != nil {
				return st, err
			}
			next++
		}
	}
	for next < st.Chunks {
		ready, ok := pending[next]
		if !ok {
			return st, fmt.Errorf("chunk %d was never produced", next)
		}
		delete(pending, next)
		if err := write(ready); err != nil {
			return st, err
		}
		next++
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return st, err
	}
	index := enc.EncodeAll(MarshalIndex(entries), nil)
	enc.Close()
	if _, err := out.Write(index); err != nil {
		return st, err
	}
	header := Header{
		ChunkSize: chunkSize, LogicalSize: info.Size(), IndexOffset: cursor,
		IndexLength: int64(len(index)), ChunkCount: st.Chunks,
		StoredChunks: st.Chunks - st.Zero - st.Deduped,
	}
	if _, err := out.WriteAt(header.marshal(), 0); err != nil {
		return st, err
	}
	if err := out.Sync(); err != nil {
		return st, err
	}
	final, err := out.Stat()
	if err != nil {
		return st, err
	}
	st.Stored = final.Size()
	st.IndexBytes = int64(len(index))
	st.Elapsed = time.Since(started)
	return st, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
