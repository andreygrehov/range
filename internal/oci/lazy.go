package oci

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/gzindex"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/virtual"
	"github.com/andreygrehov/range/internal/zstdindex"
)

// Lazy serves a container image straight from its registry, as an EROFS
// image whose file data stays in the layers.
//
// A layer is a tar stream, usually gzipped. Range reads each layer once to
// index it: where every file's body starts in the uncompressed stream, and
// checkpoints that let the gzip be entered in the middle. With the index a
// file costs a ranged read of the compressed bytes around it. The index is
// kept, so only the first open of a layer anywhere reads all of it.
type Lazy struct {
	image    string
	platform Platform
	dir      string // where layer indexes, and layers read whole, are kept

	// keepLayers keeps a layer read whole to index it, so reads never fetch
	// what was already downloaded once. Off, reads always go to the registry,
	// as they do for a layer whose index came from somewhere else.
	keepLayers bool

	mu     sync.Mutex
	opened *lazyImage

	// What ranged reads of the layers moved, compressed, for object.WireCounter.
	wireBytes, wireRequests atomic.Int64
}

// WireBytes is the layer bytes fetched since the image was opened.
func (l *Lazy) WireBytes() int64 { return l.wireBytes.Load() }

// WireRequests is the number of ranged layer reads.
func (l *Lazy) WireRequests() int64 { return l.wireRequests.Load() }

type lazyImage struct {
	digest string
	*virtual.Image
}

// NewLazy serves image for platform, keeping layer indexes in dir.
func NewLazy(image string, platform Platform, dir string) *Lazy {
	return &Lazy{image: image, platform: platform, dir: dir, keepLayers: true}
}

// imageRef is what docker pull accepts: an optional registry host, a
// repository, and a tag or digest.
var imageRef = regexp.MustCompile(`^(?:[a-zA-Z0-9][a-zA-Z0-9.-]*(?::[0-9]+)?/)?` +
	`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
	`(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)

// ImageName returns the image a URI names, and whether it names one:
// docker://name and oci://name always do, and a bare name does when it reads
// as an image reference and is not a file here.
func ImageName(uri string) (string, bool) {
	for _, scheme := range []string{"docker://", "oci://"} {
		if name, ok := strings.CutPrefix(uri, scheme); ok {
			return name, true
		}
	}
	if strings.Contains(uri, "://") || strings.HasSuffix(uri, ".range") || !imageRef.MatchString(uri) {
		return "", false
	}
	if _, err := os.Stat(uri); err == nil {
		return "", false
	}
	return uri, true
}

// Stat opens the image on first use: it resolves the tag to a manifest,
// indexes any layer not indexed yet, and lays the image out. The ETag is the
// manifest digest, so a tag that moves is a different image to the cache.
func (l *Lazy) Stat(ctx context.Context, _ string) (object.Info, error) {
	img, err := l.open(ctx)
	if err != nil {
		return object.Info{}, err
	}
	return object.Info{Size: img.Size, ETag: layoutVersion + img.digest}, nil
}

// ReadRange assembles a range of the image.
func (l *Lazy) ReadRange(ctx context.Context, _ string, offset, length int64, _, _ string) ([]byte, error) {
	img, err := l.open(ctx)
	if err != nil {
		return nil, err
	}
	return img.ReadRange(ctx, offset, length)
}

// layoutVersion is part of the identity the block cache keys on: Range lays
// the image out itself, so a change to the layout, or to what Range adds to it
// such as environment.json, must not be answered from blocks of the old one.
const layoutVersion = "range-oci-1:"

// fileAt is where a file's body starts: a layer and an offset in that layer's
// uncompressed tar stream.
type fileAt struct {
	layer  int
	offset int64
}

func (l *Lazy) open(ctx context.Context) (*lazyImage, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.opened != nil {
		return l.opened, nil
	}
	r, err := resolveCached(ctx, l.dir, l.image, l.platform)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", l.image, err)
	}
	blobs, err := os.MkdirTemp("", "range-blobs-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(blobs)
	tree := newTree(blobs)
	where := map[*erofs.Node]fileAt{}
	readers := make([]layerReader, len(r.manifest.Layers))
	l.fetchCatalogIndexes(ctx, r)
	for i, layer := range r.manifest.Layers {
		idx, err := l.layerIndex(ctx, r, i)
		if err != nil {
			return nil, fmt.Errorf("%s: layer %s: %w", l.image, layer.Digest, err)
		}
		budget := max(16<<20, segmentBudget/int64(len(r.manifest.Layers)))
		if readers[i], err = l.newLayerReader(r.client, layer.Digest, idx, budget); err != nil {
			return nil, err
		}
		for _, entry := range idx.Entries {
			offset := entry.Offset
			if err := tree.apply(entry.header(), i, func(node *erofs.Node) error {
				node.External = true
				where[node] = fileAt{layer: i, offset: offset}
				return nil
			}); err != nil {
				return nil, fmt.Errorf("%s: layer %s: %w", l.image, layer.Digest, err)
			}
		}
	}
	if err := tree.finish(l.image, r.config, l.platform); err != nil {
		return nil, err
	}
	root, nodes := tree.nodes()
	img, err := virtual.Build(root, nodes, 1<<20, func(node *erofs.Node) virtual.ReadFunc {
		at := where[node]
		reader := readers[at.layer]
		return func(ctx context.Context, dst []byte, off int64) error {
			return reader.ReadAt(ctx, dst, at.offset+off)
		}
	})
	if err != nil {
		return nil, err
	}
	l.opened = &lazyImage{digest: r.digest, Image: img}
	return l.opened, nil
}

// Entry is one tar header of a layer and where its body starts in the
// layer's uncompressed stream.
type Entry struct {
	Name, Linkname     string
	Typeflag           byte
	Mode               int64
	UID, GID           int
	ModTime            time.Time
	Size               int64
	Devmajor, Devminor int64
	Offset             int64
}

func (e Entry) header() *tar.Header {
	return &tar.Header{Name: e.Name, Linkname: e.Linkname, Typeflag: e.Typeflag, Mode: e.Mode,
		Uid: e.UID, Gid: e.GID, ModTime: e.ModTime, Size: e.Size, Devmajor: e.Devmajor, Devminor: e.Devminor}
}

// Layer kinds an index can describe.
const (
	kindGzip = "gzip" // entered through gzindex checkpoints
	kindZstd = "zstd" // entered at frame starts, or decoded forward
	kindTar  = "tar"  // uncompressed: offsets are blob offsets
)

// layerIndex is everything Range keeps about one layer.
type layerIndex struct {
	Version int
	Digest  string
	Kind    string
	Gzip    *gzindex.Index
	Zstd    *zstdindex.Index
	Entries []Entry

	// Size and the hash of every Chunk bytes of the blob as served, so a
	// ranged read of any kind of layer is checked before it is used.
	Size        int64
	Chunk       int64
	ChunkHashes [][32]byte
}

// indexVersion changes whenever layerIndex does; an index of another version
// is rebuilt.
const indexVersion = 3

func (l *Lazy) indexPath(digest string) string {
	return filepath.Join(l.dir, strings.ReplaceAll(digest, ":", "-")+".idx")
}

// blobPath is where a layer read whole is kept, compressed, as served.
func (l *Lazy) blobPath(digest string) string {
	return filepath.Join(l.dir, "blobs", strings.ReplaceAll(digest, ":", "-"))
}

// layerIndex loads layer i's index, or builds and keeps it.
func (l *Lazy) layerIndex(ctx context.Context, r resolved, i int) (*layerIndex, error) {
	layer := r.manifest.Layers[i]
	if idx, err := loadIndex(l.indexPath(layer.Digest)); err == nil && idx.Digest == layer.Digest {
		return idx, nil
	}
	// fetchCatalogIndexes already asked the catalog. Whatever it could not
	// supply is built here.
	fmt.Fprintf(os.Stderr, "Indexing %s, layer %d/%d, %s, once\n",
		l.image, i+1, len(r.manifest.Layers), bytesize.Format(layer.Size))
	keep := ""
	if l.keepLayers {
		keep = l.blobPath(layer.Digest)
	}
	idx, err := buildIndex(ctx, r.client, layer.Digest, keep)
	if err != nil {
		return nil, err
	}
	if err := saveIndex(l.indexPath(layer.Digest), idx); err != nil {
		fmt.Fprintf(os.Stderr, "range: could not keep the index of %s: %v\n", layer.Digest, err)
	}
	return idx, nil
}

// fetchCatalogIndexes asks the catalog, all at once, for the index of every
// layer that has none here yet. Each answer is a round trip to a CDN, and one
// after another they add up. A catalog that is missing a layer, unreachable
// or wrong only means that layerIndex builds that index itself.
func (l *Lazy) fetchCatalogIndexes(ctx context.Context, r resolved) {
	layers := r.manifest.Layers
	errs := make([]error, len(layers))
	var wg sync.WaitGroup
	slots := make(chan struct{}, 8)
	for i, layer := range layers {
		if idx, err := loadIndex(l.indexPath(layer.Digest)); err == nil && idx.Digest == layer.Digest {
			errs[i] = errNotInCatalog // already here: nothing to report
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			_, errs[i] = fetchIndex(ctx, layer.Digest, layer.Size, l.indexPath(layer.Digest))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		switch {
		case err == nil:
			fmt.Fprintf(os.Stderr, "Index of %s, layer %d/%d, from the catalog\n", l.image, i+1, len(layers))
		case !errors.Is(err, errNotInCatalog):
			fmt.Fprintf(os.Stderr, "range: catalog: %v; indexing it here\n", err)
		}
	}
}

// buildIndex reads a whole layer once, checks it against its digest, and
// records its tar entries and, for gzip, checkpoints. With keep set, the layer
// is also written there, and only once its digest checks out.
func buildIndex(ctx context.Context, c *client, digest, keep string) (*layerIndex, error) {
	sum, want, err := newDigestHash(digest)
	if err != nil {
		return nil, err
	}
	body, err := c.blobStream(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var source io.Reader = io.TeeReader(body, sum)
	var kept *os.File
	if keep != "" {
		if err := os.MkdirAll(filepath.Dir(keep), 0o755); err != nil {
			return nil, err
		}
		if kept, err = os.CreateTemp(filepath.Dir(keep), ".partial-*"); err != nil {
			return nil, err
		}
		defer func() {
			if kept != nil {
				kept.Close()
				os.Remove(kept.Name())
			}
		}()
		source = io.TeeReader(source, kept)
	}
	hashes := gzindex.NewChunkHasher(gzindex.DefaultChunk)
	counted := &counter{r: io.TeeReader(source, hashes)}
	blob := bufio.NewReaderSize(counted, 1<<20)
	magic, _ := blob.Peek(4)

	idx := &layerIndex{Version: indexVersion, Digest: digest}
	switch {
	case len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		idx.Kind = kindGzip
		out, wait := gzindex.Build(blob, 0, 0)
		entries, tocErr := readEntries(out)
		io.Copy(io.Discard, out) // let Build finish whatever the tar reader left
		gz, err := wait()
		if tocErr != nil {
			return nil, tocErr
		}
		if err != nil {
			return nil, err
		}
		idx.Gzip, idx.Entries = gz, entries
	case bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		idx.Kind = kindZstd
		out, wait := zstdindex.Build(blob)
		entries, tocErr := readEntries(out)
		io.Copy(io.Discard, out)
		zs, err := wait()
		if tocErr != nil {
			return nil, tocErr
		}
		if err != nil {
			return nil, err
		}
		idx.Zstd, idx.Entries = zs, entries
	default:
		idx.Kind = kindTar
		if idx.Entries, err = readEntries(blob); err != nil {
			return nil, err
		}
	}
	if _, err := io.Copy(io.Discard, blob); err != nil {
		return nil, err
	}
	idx.Size, idx.Chunk, idx.ChunkHashes = counted.n, gzindex.DefaultChunk, hashes.Sums()
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return nil, fmt.Errorf("digest mismatch: got %s", got)
	}
	if kept != nil {
		name := kept.Name()
		err := kept.Close()
		kept = nil
		if err == nil {
			err = os.Rename(name, keep)
		}
		if err != nil {
			os.Remove(name)
			fmt.Fprintf(os.Stderr, "range: could not keep layer %s: %v\n", digest, err)
		}
	}
	return idx, nil
}

// counter counts the bytes read through it.
type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// readEntries lists a tar stream's entries with the offset of each body.
// archive/tar reads a header and nothing past it, so what has gone through the
// counter when Next returns is exactly where the body begins.
func readEntries(r io.Reader) ([]Entry, error) {
	c := &counter{r: r}
	reader := tar.NewReader(c)
	var entries []Entry
	for {
		h, err := reader.Next()
		if err == io.EOF {
			return entries, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeGNUSparse || len(h.PAXRecords["GNU.sparse.map"]) > 0 ||
			h.PAXRecords["GNU.sparse.major"] != "" {
			return nil, fmt.Errorf("%s is a sparse file, which cannot be read lazily", h.Name)
		}
		entries = append(entries, Entry{
			Name: h.Name, Linkname: h.Linkname, Typeflag: h.Typeflag, Mode: h.Mode,
			UID: h.Uid, GID: h.Gid, ModTime: h.ModTime, Size: h.Size,
			Devmajor: h.Devmajor, Devminor: h.Devminor, Offset: c.n,
		})
	}
}

var indexMagic = []byte("RANGEOCI")

func saveIndex(path string, idx *layerIndex) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	w := bufio.NewWriter(temp)
	w.Write(indexMagic)
	zw, err := zstd.NewWriter(w)
	if err != nil {
		temp.Close()
		return err
	}
	if err := gob.NewEncoder(zw).Encode(idx); err != nil {
		temp.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		temp.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

func loadIndex(path string) (*layerIndex, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	magic := make([]byte, len(indexMagic))
	if _, err := io.ReadFull(f, magic); err != nil || !bytes.Equal(magic, indexMagic) {
		return nil, errors.New("not a layer index")
	}
	zr, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(1<<30))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	var idx layerIndex
	if err := gob.NewDecoder(zr).Decode(&idx); err != nil {
		return nil, err
	}
	if idx.Version != indexVersion {
		return nil, fmt.Errorf("layer index version %d, want %d", idx.Version, indexVersion)
	}
	if idx.Kind == kindGzip && idx.Gzip == nil || idx.Kind == kindZstd && idx.Zstd == nil {
		return nil, errors.New("a compressed layer index without its checkpoints")
	}
	return &idx, nil
}

// layerReader reads a layer's uncompressed stream.
type layerReader interface {
	ReadAt(ctx context.Context, p []byte, off int64) error
}

// segmentBudget bounds the decompressed gzip segments an image keeps, shared
// by its layers, with at least 16 MiB each.
const segmentBudget = 256 << 20

// keptLayer is a layer on local disk, until it fails a check.
type keptLayer struct {
	mu sync.Mutex
	f  *os.File
}

func (k *keptLayer) open() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.f != nil
}

func (k *keptLayer) read(_ context.Context, off, n int64) ([]byte, error) {
	k.mu.Lock()
	f := k.f
	k.mu.Unlock()
	if f == nil {
		return nil, os.ErrClosed
	}
	data := make([]byte, n)
	if _, err := f.ReadAt(data, off); err != nil {
		return nil, err
	}
	return data, nil
}

// drop stops using the kept copy and removes it, so the next session does not
// trip over it again.
func (k *keptLayer) drop(err error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.f == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "range: a kept layer failed a check (%v); reading it from the registry\n", err)
	os.Remove(k.f.Name())
	k.f.Close()
	k.f = nil
}

func (l *Lazy) newLayerReader(c *client, digest string, idx *layerIndex, budget int64) (layerReader, error) {
	// A layer kept from indexing is read from disk; one indexed elsewhere, or
	// evicted, from the registry. Kept bytes are checked like fetched ones,
	// and a kept layer that fails the check is dropped for the network.
	local := &keptLayer{}
	local.f, _ = os.Open(l.blobPath(digest))
	fromDisk := gzindex.Verified(local.read, idx.Size, idx.Chunk, idx.ChunkHashes)
	fetch := func(ctx context.Context, off, n int64) ([]byte, error) {
		if local.open() {
			data, err := fromDisk(ctx, off, n)
			if err == nil {
				return data, nil
			}
			local.drop(err)
		}
		data, err := c.blobRange(ctx, digest, off, n)
		if err == nil {
			l.wireBytes.Add(n)
			l.wireRequests.Add(1)
		}
		return data, err
	}
	verified := gzindex.Verified(fetch, idx.Size, idx.Chunk, idx.ChunkHashes)
	switch idx.Kind {
	case kindGzip:
		return gzindex.NewReader(idx.Gzip, fetch, budget), nil
	case kindZstd:
		// Decoded output can be as large as the layer; it goes next to the
		// cache, not to a /tmp that may live in memory.
		if err := os.MkdirAll(l.dir, 0o755); err != nil {
			return nil, err
		}
		return zstdindex.NewReader(idx.Zstd, zstdindex.Fetch(verified), l.dir)
	}
	return plainLayer(verified), nil
}

// plainLayer is an uncompressed layer: its tar offsets are blob offsets.
type plainLayer gzindex.Fetch

func (f plainLayer) ReadAt(ctx context.Context, p []byte, off int64) error {
	data, err := f(ctx, off, int64(len(p)))
	if err != nil {
		return err
	}
	copy(p, data)
	return nil
}
