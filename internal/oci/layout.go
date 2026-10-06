package oci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/andreygrehov/range/internal/virtual"
)

// An image's layout - its filesystem metadata, and where each file's bytes
// sit in which layer - follows from its manifest alone. So it is laid out
// once and kept, named by the platform manifest's digest and the layout
// version, and later sessions load it instead of reading every layer's index
// and laying the image out again. A layer's index is then read only when
// something reads that layer, which a session served from the block cache
// never does. Layouts are tens of megabytes for a large image, and a moved
// tag leaves the old one behind, so only the keptLayouts used last stay.

// keptLayouts is how many layouts stay.
const keptLayouts = 64

func (l *Lazy) layoutPath(digest string) string {
	name := strings.TrimSuffix(layoutVersion, ":") + "-" + strings.ReplaceAll(digest, ":", "-") + ".img"
	return filepath.Join(l.dir, "layouts", name)
}

// loadLayout returns the kept layout of the image r names, reading through
// layers, or false. A layout that does not load is removed, and built anew.
func (l *Lazy) loadLayout(r resolved, layers *layerSet) (*virtual.Image, bool) {
	path := l.layoutPath(r.digest)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	img, err := virtual.Load(data, layers.readFunc)
	if err != nil {
		os.Remove(path)
		return nil, false
	}
	markUsed(path)
	return img, true
}

// saveLayout keeps a layout for the next session. It only saves time, so a
// failure to keep it is not an error.
func (l *Lazy) saveLayout(digest string, img *virtual.Image) {
	path := l.layoutPath(digest)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	err = img.Save(tmp)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil && os.Rename(tmp.Name(), path) == nil {
		pruneLayouts(filepath.Dir(path), keptLayouts)
	}
}

// pruneLayouts keeps the keep layouts used last.
func pruneLayouts(dir string, keep int) {
	paths, _ := filepath.Glob(filepath.Join(dir, "*.img"))
	if len(paths) <= keep {
		return
	}
	used := map[string]time.Time{}
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			used[p] = info.ModTime()
		}
	}
	sort.Slice(paths, func(i, j int) bool { return used[paths[i]].After(used[paths[j]]) })
	for _, old := range paths[keep:] {
		os.Remove(old)
	}
}

// layerSet opens the reader of each layer of an image when it is first
// needed. A layer that fails to open is tried again on the next read: a
// catalog or a registry that did not answer once may answer later.
type layerSet struct {
	l       *Lazy
	r       resolved
	mu      []sync.Mutex
	readers []layerReader
}

func newLayerSet(l *Lazy, r resolved) *layerSet {
	n := len(r.manifest.Layers)
	return &layerSet{l: l, r: r, mu: make([]sync.Mutex, n), readers: make([]layerReader, n)}
}

// budget is each layer's share of the image's decompressed segments.
func (s *layerSet) budget() int64 {
	return max(16<<20, segmentBudget/int64(len(s.readers)))
}

// put records a reader already opened, as laying the image out does.
func (s *layerSet) put(i int, reader layerReader) {
	s.mu[i].Lock()
	s.readers[i] = reader
	s.mu[i].Unlock()
}

func (s *layerSet) reader(ctx context.Context, i int) (layerReader, error) {
	s.mu[i].Lock()
	defer s.mu[i].Unlock()
	if s.readers[i] != nil {
		return s.readers[i], nil
	}
	layer := s.r.manifest.Layers[i]
	idx, err := s.l.layerIndex(ctx, s.r, i)
	if err != nil {
		return nil, fmt.Errorf("%s: layer %s: %w", s.l.image, layer.Digest, err)
	}
	reader, err := s.l.newLayerReader(s.r.client, layer.Digest, idx, s.budget())
	if err != nil {
		return nil, err
	}
	s.readers[i] = reader
	return reader, nil
}

// readFunc reads the file a tag names: Tag{layer, offset in the layer}.
func (s *layerSet) readFunc(tag virtual.Tag) virtual.ReadFunc {
	layer, start := int(tag[0]), tag[1]
	return func(ctx context.Context, dst []byte, off int64) error {
		if layer < 0 || layer >= len(s.readers) {
			return fmt.Errorf("%s: a file names layer %d of %d", s.l.image, layer, len(s.readers))
		}
		reader, err := s.reader(ctx, layer)
		if err != nil {
			return err
		}
		return reader.ReadAt(ctx, dst, start+off)
	}
}
