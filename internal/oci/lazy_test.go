package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/erofs/erofstest"
)

// fakeRegistry serves one single-platform image: a manifest, a config and
// layer blobs that honour Range, and counts what it sends.
type fakeRegistry struct {
	blobs  map[string][]byte
	config string           // digest of the config blob, which is not counted
	cut    map[string]int64 // drop the next whole read of a blob after this many bytes

	mu        sync.Mutex
	manifest  []byte
	fullGets  int   // blob GETs without a Range header
	rangeGets int   // blob GETs with one
	bytesSent int64 // blob bytes, either way
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/v2/":
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(r.URL.Path, "/v2/test/app/manifests/"):
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Write(f.manifest)
	case strings.HasPrefix(r.URL.Path, "/v2/test/app/blobs/"):
		digest := strings.TrimPrefix(r.URL.Path, "/v2/test/app/blobs/")
		blob, ok := f.blobs[digest]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if digest == f.config {
			w.Write(blob)
			return
		}
		f.mu.Lock()
		if r.Header.Get("Range") == "" {
			f.fullGets++
		} else {
			f.rangeGets++
		}
		cut, drop := f.cut[digest]
		if drop && r.Header.Get("Range") == "" {
			delete(f.cut, digest)
		} else {
			drop = false
		}
		f.mu.Unlock()
		if drop {
			// Promise the whole blob, send part of it, and hang up.
			w.Header().Set("Content-Length", fmt.Sprint(len(blob)))
			w.WriteHeader(http.StatusOK)
			w.Write(blob[:cut])
			w.(http.Flusher).Flush()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		cw := &countingWriter{ResponseWriter: w, f: f}
		http.ServeContent(cw, r, "", time.Time{}, bytes.NewReader(blob))
	default:
		http.NotFound(w, r)
	}
}

type countingWriter struct {
	http.ResponseWriter
	f *fakeRegistry
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.f.mu.Lock()
	c.f.bytesSent += int64(len(p))
	c.f.mu.Unlock()
	return c.ResponseWriter.Write(p)
}

func (f *fakeRegistry) reset() {
	f.mu.Lock()
	f.fullGets, f.rangeGets, f.bytesSent = 0, 0, 0
	f.mu.Unlock()
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	w.Write(b)
	w.Close()
	return out.Bytes()
}

func randomString(seed int64, n int) string {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return string(b)
}

// serveImage starts a registry holding layers and returns the image name.
func serveImage(t *testing.T, layers ...[]byte) (*fakeRegistry, string) {
	t.Helper()
	f := &fakeRegistry{blobs: map[string][]byte{}}
	config := []byte(`{"architecture":"` + HostPlatform().arch + `","os":"linux","config":{"Env":["PATH=/usr/bin:/bin"],"WorkingDir":"/app"}}`)
	f.blobs[digestOf(config)] = config
	f.config = digestOf(config)
	type desc struct {
		MediaType string `json:"mediaType"`
		Digest    string `json:"digest"`
		Size      int64  `json:"size"`
	}
	var layerDescs []desc
	for _, layer := range layers {
		f.blobs[digestOf(layer)] = layer
		mediaType := "application/vnd.oci.image.layer.v1.tar"
		switch {
		case len(layer) > 2 && layer[0] == 0x1f && layer[1] == 0x8b:
			mediaType += "+gzip"
		case len(layer) > 4 && bytes.Equal(layer[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}):
			mediaType += "+zstd"
		}
		layerDescs = append(layerDescs, desc{mediaType, digestOf(layer), int64(len(layer))})
	}
	f.manifest, _ = json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.manifest.v1+json",
		"config": desc{"application/vnd.oci.image.config.v1+json", digestOf(config), int64(len(config))},
		"layers": layerDescs,
	})
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("RANGE_INDEX_URL", "off") // tests that want a catalog serve their own
	old := transport
	transport = srv.Client().Transport
	t.Cleanup(func() { transport = old })
	return f, strings.TrimPrefix(srv.URL, "https://") + "/test/app:1.0"
}

// readAll reads a whole virtual image the way the block cache would.
func readAll(t *testing.T, l *Lazy) erofstest.Image {
	t.Helper()
	info, err := l.Stat(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var all []byte
	for off := int64(0); off < info.Size; off += 1 << 20 {
		part, err := l.ReadRange(context.Background(), "", off, min(1<<20, info.Size-off), "", "")
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		all = append(all, part...)
	}
	return erofstest.Image{T: t, Bytes: all}
}

func find(img erofstest.Image, p string) (erofstest.Inode, bool) {
	nid := uint64(binary.LittleEndian.Uint16(img.Bytes[erofs.SuperOffset+14:]))
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		found := false
		for _, e := range img.Readdir(img.Inode(nid)) {
			if e.Name == part {
				nid, found = e.Nid, true
				break
			}
		}
		if !found {
			return erofstest.Inode{}, false
		}
	}
	return img.Inode(nid), true
}

func testLayers(t *testing.T) (layers [][]byte, want map[string]string, gone []string) {
	big := randomString(1, 3<<20+17)
	base := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755}, ""),
		tarEntry(t, &tar.Header{Name: "bin/sh", Typeflag: tar.TypeReg, Mode: 0o755}, "#!fake shell\n"),
		tarEntry(t, &tar.Header{Name: "usr/lib/big.so", Typeflag: tar.TypeReg, Mode: 0o644}, big),
		tarEntry(t, &tar.Header{Name: "etc/os-release", Typeflag: tar.TypeReg, Mode: 0o644}, "ID=test\n"),
		tarEntry(t, &tar.Header{Name: "etc/old.conf", Typeflag: tar.TypeReg, Mode: 0o644}, "going away\n"),
		tarEntry(t, &tar.Header{Name: "usr/lib/link.so", Typeflag: tar.TypeLink, Linkname: "usr/lib/big.so"}, ""),
		tarEntry(t, &tar.Header{Name: "usr/bin/sh", Typeflag: tar.TypeSymlink, Linkname: "/bin/sh"}, ""),
		tarEntry(t, &tar.Header{Name: "empty", Typeflag: tar.TypeReg, Mode: 0o644}, ""),
	)
	var many []func(*tar.Writer)
	for i := 0; i < 300; i++ { // small files packed together, as in a real layer
		many = append(many, tarEntry(t, &tar.Header{Name: fmt.Sprintf("app/f%03d.py", i), Typeflag: tar.TypeReg, Mode: 0o644},
			fmt.Sprintf("print(%d)\n", i)+strings.Repeat("#", i*7)))
	}
	many = append(many,
		tarEntry(t, &tar.Header{Name: "etc/.wh.old.conf", Typeflag: tar.TypeReg}, ""),
		tarEntry(t, &tar.Header{Name: "etc/os-release", Typeflag: tar.TypeReg, Mode: 0o644}, "ID=test\nVERSION=2\n"),
	)
	top := tarLayer(t, many...)
	want = map[string]string{
		"bin/sh": "#!fake shell\n", "usr/lib/big.so": big, "usr/lib/link.so": big,
		"etc/os-release": "ID=test\nVERSION=2\n", "empty": "",
	}
	for i := 0; i < 300; i++ {
		want[fmt.Sprintf("app/f%03d.py", i)] = fmt.Sprintf("print(%d)\n", i) + strings.Repeat("#", i*7)
	}
	return [][]byte{gz(t, base.Bytes()), gz(t, top.Bytes())}, want, []string{"etc/old.conf"}
}

func TestLazyImageMatchesTheLayers(t *testing.T) {
	layers, want, gone := testLayers(t)
	_, name := serveImage(t, layers...)
	img := readAll(t, NewLazy(name, HostPlatform(), t.TempDir()))
	for p, body := range want {
		in, ok := find(img, p)
		if !ok {
			t.Errorf("%s is missing", p)
			continue
		}
		if in.Size != int64(len(body)) || (len(body) > 0 && string(img.Data(in)) != body) {
			t.Errorf("%s reads back wrong", p)
		}
	}
	for _, p := range gone {
		if _, ok := find(img, p); ok {
			t.Errorf("%s survived its whiteout", p)
		}
	}
	if in, ok := find(img, "usr/bin/sh"); !ok || string(img.Data(in)) != "/bin/sh" {
		t.Error("the symlink is wrong")
	}
	env, ok := find(img, "etc/range/environment.json")
	if !ok || !strings.Contains(string(img.Data(env)), `"workdir": "/app"`) {
		t.Error("environment.json is missing or does not carry the image config")
	}
}

func TestASecondOpenUsesTheIndexAndReadsOnlyWhatItNeeds(t *testing.T) {
	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if reg.fullGets != len(layers) {
		t.Fatalf("indexing made %d full blob reads, want one per layer", reg.fullGets)
	}

	// As if the index had come from somewhere else: the layers are not here.
	if err := os.RemoveAll(filepath.Join(dir, "blobs")); err != nil {
		t.Fatal(err)
	}
	reg.reset()
	l := NewLazy(name, HostPlatform(), dir)
	info, err := l.Stat(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if reg.fullGets != 0 || reg.bytesSent != 0 {
		t.Fatalf("an indexed image read %d blobs (%d bytes) just to open", reg.fullGets, reg.bytesSent)
	}
	// Read one small file through the image: find its extent by size.
	target := want["etc/os-release"]
	var hit bool
	for _, e := range l.opened.Extents {
		if e.Size != int64(len(target)) {
			continue
		}
		got, err := l.ReadRange(context.Background(), "", e.Start, e.Size, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) == target {
			hit = true
			break
		}
	}
	if !hit {
		t.Fatal("no extent held etc/os-release")
	}
	total := int64(0)
	for _, layer := range layers {
		total += int64(len(layer))
	}
	if reg.fullGets != 0 || reg.bytesSent > total/4 {
		t.Errorf("reading an 18-byte file sent %d of %d blob bytes (%d full reads)", reg.bytesSent, total, reg.fullGets)
	}
	if !strings.HasPrefix(info.ETag, layoutVersion+"sha256:") {
		t.Errorf("ETag %q is not the manifest digest", info.ETag)
	}
}

func TestATamperedIndexOrBlobIsNotTrusted(t *testing.T) {
	layers, _, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	// The registry now serves different bytes under the same digest.
	for d, b := range reg.blobs {
		if bytes.Equal(b, layers[0]) {
			bad := append([]byte(nil), b...)
			for i := len(bad) / 3; i < len(bad)/3+100; i++ {
				bad[i] ^= 0xff
			}
			reg.blobs[d] = bad
		}
	}
	os.RemoveAll(filepath.Join(dir, "blobs")) // read from the registry, not the kept copy
	l := NewLazy(name, HostPlatform(), dir)
	info, err := l.Stat(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for off := int64(0); off < info.Size; off += 1 << 20 {
		if _, err := l.ReadRange(context.Background(), "", off, min(1<<20, info.Size-off), "", ""); err != nil {
			failed = true
			break
		}
	}
	if !failed {
		t.Fatal("changed layer bytes were served without an error")
	}
}

func TestImageName(t *testing.T) {
	for uri, want := range map[string]string{
		"rust:1.82": "rust:1.82", "python:3.12": "python:3.12", "eclipse-temurin:21": "eclipse-temurin:21",
		"ubuntu": "ubuntu", "ghcr.io/org/app:v1": "ghcr.io/org/app:v1", "localhost:5000/app": "localhost:5000/app",
		"docker://alpine": "alpine", "oci://quay.io/x/y:1": "quay.io/x/y:1",
		"library/golang@sha256:" + strings.Repeat("a", 64): "library/golang@sha256:" + strings.Repeat("a", 64),
	} {
		if got, ok := ImageName(uri); !ok || got != want {
			t.Errorf("%s: got %q %v, want %q", uri, got, ok, want)
		}
	}
	for _, uri := range []string{"s3://b/k", "https://x/y.range", "hf://org/m", "go.range", "./local.img", "/abs/path", "Upper:tag", "a b"} {
		if _, ok := ImageName(uri); ok {
			t.Errorf("%s read as an image", uri)
		}
	}
}

// A first run downloads each layer once to index it. Nothing it downloaded is
// fetched again, in this session or the next.
func TestLayersReadWholeAreNotFetchedAgain(t *testing.T) {
	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	dir := t.TempDir()
	first := NewLazy(name, HostPlatform(), dir)
	img := readAll(t, first)
	if reg.fullGets != len(layers) || reg.rangeGets != 0 {
		t.Fatalf("first run: %d full and %d ranged blob reads, want %d and 0", reg.fullGets, reg.rangeGets, len(layers))
	}
	if first.WireBytes() != 0 {
		t.Errorf("first run fetched %d bytes again after indexing", first.WireBytes())
	}
	if in, ok := find(img, "usr/lib/big.so"); !ok || string(img.Data(in)) != want["usr/lib/big.so"] {
		t.Fatal("a file read from a kept layer is wrong")
	}

	reg.reset()
	readAll(t, NewLazy(name, HostPlatform(), dir))
	if reg.fullGets != 0 || reg.rangeGets != 0 {
		t.Errorf("second run: %d full and %d ranged blob reads, want none", reg.fullGets, reg.rangeGets)
	}
}

func TestALayerThatFailsItsDigestIsNotKept(t *testing.T) {
	layers, _, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	for d, b := range reg.blobs {
		if bytes.Equal(b, layers[1]) {
			bad := append([]byte(nil), b...)
			bad[len(bad)-1] ^= 0xff // the gzip trailer: the tar still parses
			reg.blobs[d] = bad
		}
	}
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err == nil {
		t.Fatal("a layer that fails its digest was accepted")
	}
	kept, _ := os.ReadDir(filepath.Join(dir, "blobs"))
	for _, e := range kept {
		if e.Name() == strings.ReplaceAll(digestOf(layers[1]), ":", "-") || strings.HasPrefix(e.Name(), ".partial") {
			t.Errorf("%s was kept", e.Name())
		}
	}
}

// An uncompressed layer is read by plain ranges, and those are checked too.
func TestAnUncompressedLayerIsReadAndChecked(t *testing.T) {
	big := randomString(5, 2<<20+3)
	layer := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "bin/sh", Typeflag: tar.TypeReg, Mode: 0o755}, "#!sh\n"),
		tarEntry(t, &tar.Header{Name: "data/big", Typeflag: tar.TypeReg, Mode: 0o644}, big),
	).Bytes()
	reg, name := serveImage(t, layer)
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	os.RemoveAll(filepath.Join(dir, "blobs"))
	img := readAll(t, NewLazy(name, HostPlatform(), dir))
	if in, ok := find(img, "data/big"); !ok || string(img.Data(in)) != big {
		t.Fatal("an uncompressed layer reads back wrong")
	}

	for d, b := range reg.blobs {
		if bytes.Equal(b, layer) {
			bad := append([]byte(nil), b...)
			bad[len(bad)/2] ^= 0xff
			reg.blobs[d] = bad
		}
	}
	l := NewLazy(name, HostPlatform(), dir)
	info, err := l.Stat(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for off := int64(0); off < info.Size; off += 1 << 20 {
		if _, err := l.ReadRange(context.Background(), "", off, min(1<<20, info.Size-off), "", ""); err != nil {
			failed = true
		}
	}
	if !failed {
		t.Fatal("a changed byte in an uncompressed layer was served")
	}
}

// zstd layers: one frame, as docker buildx writes them, and many frames, as
// zstd:chunked does, in one image with a gzip layer between them.
func TestZstdLayers(t *testing.T) {
	big := randomString(7, 20<<20) // over the whole-frame limit: decoded forward
	one := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "bin/sh", Typeflag: tar.TypeReg, Mode: 0o755}, "#!sh\n"),
		tarEntry(t, &tar.Header{Name: "opt/big", Typeflag: tar.TypeReg, Mode: 0o644}, big),
		tarEntry(t, &tar.Header{Name: "opt/after", Typeflag: tar.TypeReg, Mode: 0o644}, "after the big one\n"),
	).Bytes()
	var oneFrame bytes.Buffer
	w, _ := zstd.NewWriter(&oneFrame, zstd.WithEncoderConcurrency(1))
	w.Write(one)
	w.Close()

	chunked := tarLayer(t,
		tarEntry(t, &tar.Header{Name: "etc/a", Typeflag: tar.TypeReg, Mode: 0o644}, strings.Repeat("a", 70000)),
		tarEntry(t, &tar.Header{Name: "etc/b", Typeflag: tar.TypeReg, Mode: 0o644}, "b\n"),
	).Bytes()
	enc, _ := zstd.NewWriter(nil)
	var many []byte
	for off := 0; off < len(chunked); off += 16 << 10 { // a frame every 16 KiB
		many = enc.EncodeAll(chunked[off:min(off+16<<10, len(chunked))], many)
	}

	middle := gz(t, tarLayer(t, tarEntry(t, &tar.Header{Name: "etc/c", Typeflag: tar.TypeReg, Mode: 0o644}, "c\n")).Bytes())
	_, name := serveImage(t, oneFrame.Bytes(), middle, many)
	want := map[string]string{"opt/big": big, "opt/after": "after the big one\n",
		"etc/a": strings.Repeat("a", 70000), "etc/b": "b\n", "etc/c": "c\n"}

	dir := t.TempDir()
	for _, keep := range []bool{true, false} {
		l := NewLazy(name, HostPlatform(), dir)
		if !keep {
			os.RemoveAll(filepath.Join(dir, "blobs"))
			l.keepLayers = false
		}
		img := readAll(t, l)
		for p, body := range want {
			in, ok := find(img, p)
			if !ok || string(img.Data(in)) != body {
				t.Errorf("keep=%v: %s reads back wrong", keep, p)
			}
		}
	}
}

func TestEnvironmentCarriesTheImageCommand(t *testing.T) {
	exists := func(string) bool { return true }
	var llama ImageConfig
	llama.Config.Entrypoint = []string{"/app/llama-cli"}
	llama.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
	meta := environmentFor("llama", llama, HostPlatform(), exists)
	if meta.Environment["PATH"] != "/usr/local/bin:/usr/bin:/bin:/app" {
		t.Errorf("PATH %q, want the entrypoint's directory added", meta.Environment["PATH"])
	}
	if len(meta.Entrypoint) != 1 || meta.Entrypoint[0] != "/app/llama-cli" {
		t.Errorf("entrypoint %q", meta.Entrypoint)
	}

	var python ImageConfig
	python.Config.Cmd = []string{"python3"}
	python.Config.Env = []string{"PATH=/usr/local/bin:/usr/bin"}
	meta = environmentFor("python", python, HostPlatform(), exists)
	if meta.Environment["PATH"] != "/usr/local/bin:/usr/bin" || len(meta.Cmd) != 1 {
		t.Errorf("an image without an entrypoint changed PATH (%q) or lost its cmd (%q)", meta.Environment["PATH"], meta.Cmd)
	}

	var inPath ImageConfig
	inPath.Config.Entrypoint = []string{"/usr/bin/tini", "--"}
	inPath.Config.Env = []string{"PATH=/usr/bin:/bin"}
	if got := environmentFor("x", inPath, HostPlatform(), exists).Environment["PATH"]; got != "/usr/bin:/bin" {
		t.Errorf("PATH %q: a directory already on PATH was added again", got)
	}
}

// A kept layer that no longer matches its hashes is dropped and read from the
// registry instead, instead of failing every read until the cache is cleared.
func TestACorruptKeptLayerFallsBackToTheRegistry(t *testing.T) {
	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	dir := t.TempDir()
	if _, err := NewLazy(name, HostPlatform(), dir).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dir, "blobs", strings.ReplaceAll(digestOf(layers[0]), ":", "-"))
	b, err := os.ReadFile(kept)
	if err != nil {
		t.Fatal(err)
	}
	for i := len(b) / 4; i < len(b)/4+4096; i++ {
		b[i] ^= 0xff
	}
	os.WriteFile(kept, b, 0o644)
	reg.reset()
	img := readAll(t, NewLazy(name, HostPlatform(), dir))
	if in, ok := find(img, "usr/lib/big.so"); !ok || string(img.Data(in)) != want["usr/lib/big.so"] {
		t.Fatal("a corrupt kept layer was served, or its file reads back wrong")
	}
	if reg.rangeGets == 0 {
		t.Error("nothing was fetched from the registry after the kept copy failed")
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Error("the corrupt kept layer is still there")
	}
}

// A registry or CDN that drops the connection half way through a layer costs
// a resumed request, not the layer.
func TestADroppedLayerDownloadResumes(t *testing.T) {
	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	reg.cut = map[string]int64{digestOf(layers[0]): int64(len(layers[0]) / 3)}
	img := readAll(t, NewLazy(name, HostPlatform(), t.TempDir()))
	if in, ok := find(img, "usr/lib/big.so"); !ok || string(img.Data(in)) != want["usr/lib/big.so"] {
		t.Fatal("a file from the resumed layer reads back wrong")
	}
	if reg.rangeGets == 0 {
		t.Error("the dropped download was not resumed with a ranged request")
	}
}

// serveCatalog serves dir as a catalog and points Range at it.
func serveCatalog(t *testing.T, dir string) *atomic.Int64 {
	t.Helper()
	hits := new(atomic.Int64)
	files := http.FileServer(http.Dir(dir))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("RANGE_INDEX_URL", srv.URL)
	return hits
}

// With the layers in a catalog, the very first run of an image reads no layer
// whole: it fetches the indexes and then only what it touches.
func TestAFirstRunWithACatalogIsLazy(t *testing.T) {
	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	published := t.TempDir()
	if err := IndexImage(context.Background(), name, HostPlatform(), published, false, func(string) {}); err != nil {
		t.Fatal(err)
	}
	reg.reset()
	hits := serveCatalog(t, published)

	l := NewLazy(name, HostPlatform(), t.TempDir())
	if _, err := l.Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if reg.fullGets != 0 {
		t.Errorf("a first run with a catalog read %d layers whole", reg.fullGets)
	}
	if hits.Load() != int64(len(layers)) {
		t.Errorf("%d catalog requests, want one per layer", hits.Load())
	}
	img := readAll(t, l)
	if in, ok := find(img, "usr/lib/big.so"); !ok || string(img.Data(in)) != want["usr/lib/big.so"] {
		t.Fatal("a file read through a catalog index is wrong")
	}
}

// A catalog can only save work. An index for another layer, or one that is
// not an index at all, is refused, and Range indexes the layer itself.
func TestAWrongCatalogIndexIsRefused(t *testing.T) {
	layers, want, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	published := t.TempDir()
	if err := IndexImage(context.Background(), name, HostPlatform(), published, false, func(string) {}); err != nil {
		t.Fatal(err)
	}
	// Swap the two layers' indexes, and break nothing else.
	a := filepath.Join(published, filepath.FromSlash(catalogName(digestOf(layers[0]))))
	b := filepath.Join(published, filepath.FromSlash(catalogName(digestOf(layers[1]))))
	ab, _ := os.ReadFile(a)
	bb, _ := os.ReadFile(b)
	os.WriteFile(a, bb, 0o644)
	os.WriteFile(b, ab, 0o644)
	reg.reset()
	serveCatalog(t, published)

	img := readAll(t, NewLazy(name, HostPlatform(), t.TempDir()))
	if reg.fullGets != len(layers) {
		t.Errorf("%d layers indexed here, want both, since the catalog's indexes were for other layers", reg.fullGets)
	}
	if in, ok := find(img, "usr/lib/big.so"); !ok || string(img.Data(in)) != want["usr/lib/big.so"] {
		t.Fatal("a file reads back wrong after refusing the catalog")
	}
}

func TestAMissingCatalogEntryFallsBack(t *testing.T) {
	layers, _, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	serveCatalog(t, t.TempDir()) // empty: every lookup is a 404
	if _, err := NewLazy(name, HostPlatform(), t.TempDir()).Stat(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if reg.fullGets != len(layers) {
		t.Errorf("%d layers indexed, want %d", reg.fullGets, len(layers))
	}
}

func TestIndexImageSkipsWhatIsPublished(t *testing.T) {
	layers, _, _ := testLayers(t)
	reg, name := serveImage(t, layers...)
	published := t.TempDir()
	if err := IndexImage(context.Background(), name, HostPlatform(), published, false, func(string) {}); err != nil {
		t.Fatal(err)
	}
	serveCatalog(t, published)
	reg.reset()
	var lines []string
	if err := IndexImage(context.Background(), name, HostPlatform(), t.TempDir(), true, func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatal(err)
	}
	if reg.fullGets != 0 {
		t.Errorf("re-indexed %d published layers", reg.fullGets)
	}
	for _, line := range lines {
		if !strings.Contains(line, "already in the catalog") {
			t.Errorf("unexpected: %s", line)
		}
	}
}

func TestCatalogName(t *testing.T) {
	d := "sha256:" + strings.Repeat("ab", 32)
	if got := catalogName(d); got != "ab/sha256-"+strings.Repeat("ab", 32)+".idx" {
		t.Errorf("got %s", got)
	}
	for _, bad := range []string{"sha512:" + strings.Repeat("a", 128), "sha256:abc", "nonsense"} {
		if catalogName(bad) != "" {
			t.Errorf("%s got a catalog name", bad)
		}
	}
}

func TestCatalogProfiles(t *testing.T) {
	hex := strings.Repeat("cd", 32)
	etag := layoutVersion + "sha256:" + hex
	name := CatalogProfileName(etag)
	if name != "cd/range-oci-1-sha256-"+hex+".profile.json" {
		t.Fatalf("name %q", name)
	}
	for _, bad := range []string{"sha256:" + hex, "range-oci-1:sha256:abc", "some-etag"} {
		if CatalogProfileName(bad) != "" {
			t.Errorf("%q got a catalog profile name", bad)
		}
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "cd"), 0o755)
	os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(`{"version":1}`), 0o644)
	serveCatalog(t, dir)
	data, err := FetchCatalogProfile(context.Background(), etag)
	if err != nil || string(data) != `{"version":1}` {
		t.Fatalf("got %q, %v", data, err)
	}
	if _, err := FetchCatalogProfile(context.Background(), layoutVersion+"sha256:"+strings.Repeat("ef", 32)); !errors.Is(err, errNotInCatalog) {
		t.Errorf("a missing profile: %v, want errNotInCatalog", err)
	}
}
