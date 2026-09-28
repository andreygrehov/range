package gzindex

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
)

// ChunkHasher hashes a stream in fixed chunks as it is written.
type ChunkHasher struct {
	chunk  int64
	sum    hash.Hash
	n      int64
	hashes [][sha256.Size]byte
}

// NewChunkHasher hashes every chunk bytes of what is written to it.
func NewChunkHasher(chunk int64) *ChunkHasher {
	if chunk <= 0 {
		chunk = DefaultChunk
	}
	return &ChunkHasher{chunk: chunk, sum: sha256.New()}
}

func (h *ChunkHasher) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		n := min(int64(len(p)), h.chunk-h.n%h.chunk)
		h.sum.Write(p[:n])
		h.n += n
		p = p[n:]
		if h.n%h.chunk == 0 {
			h.close()
		}
	}
	return written, nil
}

func (h *ChunkHasher) close() {
	var s [sha256.Size]byte
	copy(s[:], h.sum.Sum(nil))
	h.hashes = append(h.hashes, s)
	h.sum.Reset()
}

// Sums closes the last partial chunk and returns every chunk's hash.
func (h *ChunkHasher) Sums() [][sha256.Size]byte {
	if h.n%h.chunk != 0 {
		h.close()
		h.n += h.chunk - h.n%h.chunk // closed: a later Sums adds nothing
	}
	return h.hashes
}

// Verified wraps fetch so every byte it returns has been checked against the
// chunk hashes of a stream of size bytes: a range is widened to whole chunks,
// fetched, checked, and cut back to what was asked for.
func Verified(fetch Fetch, size, chunk int64, sums [][sha256.Size]byte) Fetch {
	return func(ctx context.Context, off, n int64) ([]byte, error) {
		if off < 0 || n < 0 || off+n > size {
			return nil, fmt.Errorf("gzindex: range %d+%d outside %d bytes", off, n, size)
		}
		lo := off / chunk * chunk
		hi := min(size, (off+n+chunk-1)/chunk*chunk)
		data, err := fetch(ctx, lo, hi-lo)
		if err != nil {
			return nil, err
		}
		if int64(len(data)) != hi-lo {
			return nil, fmt.Errorf("gzindex: fetched %d bytes, wanted %d", len(data), hi-lo)
		}
		for c := lo; c < hi; c += chunk {
			i := c / chunk
			if i >= int64(len(sums)) {
				return nil, errors.New("gzindex: fewer chunk hashes than chunks")
			}
			if sha256.Sum256(data[c-lo:min(c+chunk, hi)-lo]) != sums[i] {
				return nil, fmt.Errorf("gzindex: bytes at %d do not match their hash", c)
			}
		}
		return data[off-lo : off-lo+n], nil
	}
}
