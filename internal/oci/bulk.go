package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A layer read by ranged requests costs a round trip for each piece. That is
// what makes a lazy start fast, and what makes a workload that reads most of
// a layer slower than docker pull, which downloads layers whole and several
// at once. So once a session has fetched an eighth of a large layer, Range
// downloads the whole layer in the background, checks it against its digest
// and keeps it, as it keeps a layer it indexed itself. While it downloads, a
// read of the part already written comes from the file, checked against the
// chunk hashes in the index, and only a read beyond it goes to the registry.
// A small layer is read in a few requests anyway, and is left to them.
const (
	bulkFraction = 8 // a layer is downloaded whole after 1/bulkFraction of it
	bulkParallel = 3 // layers downloaded at once, as docker pull does
)

// bulkMinSize is the smallest layer downloaded whole. A variable, for tests.
var bulkMinSize int64 = 32 << 20

// bulkLayer watches the ranged reads of one layer.
type bulkLayer struct {
	l       *Lazy
	c       *client
	digest  string
	size    int64 // compressed, as served
	local   *keptLayer
	fetched atomic.Int64
	started atomic.Bool

	mu      sync.Mutex
	partial *os.File     // the download so far, while it runs
	written atomic.Int64 // how much of it is in the file
}

var errNotYet = errors.New("not downloaded yet")

// readPartial reads from the download in progress, once it has got that far.
func (b *bulkLayer) readPartial(_ context.Context, off, n int64) ([]byte, error) {
	if off+n > b.written.Load() {
		return nil, errNotYet
	}
	b.mu.Lock()
	f := b.partial
	b.mu.Unlock()
	if f == nil {
		return nil, errNotYet
	}
	data := make([]byte, n)
	if _, err := f.ReadAt(data, off); err != nil {
		return nil, err
	}
	return data, nil
}

// progress follows keepBlob: first the file it writes, then how far it got.
func (b *bulkLayer) progress(path string, written int64) {
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return
		}
		b.mu.Lock()
		b.partial = f
		b.mu.Unlock()
		return
	}
	b.written.Store(written)
}

// fetchedRange counts n bytes read from the registry, and starts the whole
// download once they reach the layer's threshold.
func (b *bulkLayer) fetchedRange(n int64) {
	if !b.l.keepLayers || b.size < bulkMinSize {
		return
	}
	if b.fetched.Add(n)*bulkFraction < b.size || !b.started.CompareAndSwap(false, true) {
		return
	}
	b.l.bulk.Add(1)
	go b.download()
}

func (b *bulkLayer) download() {
	defer b.l.bulk.Done()
	slots := b.l.bulkSlots()
	slots <- struct{}{}
	defer func() { <-slots }()
	_, err := b.l.keepBlob(context.Background(), b.c, b.digest, b.progress)
	b.mu.Lock()
	// The file read during the download is the kept layer once renamed.
	partial := b.partial
	b.partial = nil
	b.mu.Unlock()
	switch {
	case err != nil && partial != nil:
		partial.Close() // the layer is read from the registry, as before
	case err == nil && partial != nil:
		b.local.adopt(partial)
	}
}

// keepBlob downloads a layer whole into the blob store, checked against its
// digest, and returns where it is. progress, if given, hears the path of the
// file being written, then the bytes written so far.
func (l *Lazy) keepBlob(ctx context.Context, c *client, digest string, progress func(string, int64)) (string, error) {
	want, ok := strings.CutPrefix(digest, "sha256:")
	if !ok {
		return "", fmt.Errorf("blob %s: only sha256 digests are kept", digest)
	}
	path := l.blobPath(digest)
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	removeStalePartials(dir)
	tmp, err := os.CreateTemp(dir, ".partial-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if progress == nil {
		progress = func(string, int64) {}
	}
	progress(tmp.Name(), 0)
	stream, err := c.blobStream(ctx, digest)
	if err != nil {
		tmp.Close()
		return "", err
	}
	sum := sha256.New()
	_, err = io.Copy(io.MultiWriter(&progressWriter{w: tmp, report: progress}, sum), stream)
	stream.Close()
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("blob %s: %w", digest, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return "", fmt.Errorf("blob %s: downloaded bytes hash to %s", digest, got)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	l.evictKept(path)
	return path, nil
}

// progressWriter reports the bytes written through it.
type progressWriter struct {
	w      io.Writer
	n      int64
	report func(string, int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	p.report("", p.n)
	return n, err
}

// removeStalePartials deletes downloads that a session killed half way left
// behind. A download in progress is younger than an hour.
func removeStalePartials(dir string) {
	partials, _ := filepath.Glob(filepath.Join(dir, ".partial-*"))
	for _, p := range partials {
		if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.Remove(p)
		}
	}
}

// bulkSlots bounds the layers of this image downloaded at once.
func (l *Lazy) bulkSlots() chan struct{} {
	l.bulkOnce.Do(func() { l.slots = make(chan struct{}, bulkParallel) })
	return l.slots
}
