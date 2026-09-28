package hub

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/erofs/erofstest"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/virtual"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// fakeHub answers the three kinds of request Range makes: the revision, the
// paged tree listing, and resolve, which redirects to a CDN path that serves
// ranges. It records every file range it serves.
type fakeHub struct {
	files map[string][]byte
	dirs  []string

	mu        sync.Mutex
	served    []string // paths the CDN served bytes of
	resolves  int      // requests to resolve URLs
	cdnExpiry string   // Expires= on CDN URLs; empty for none
	revoked   bool     // the CDN refuses every URL it handed out before
}

func newFakeHub(t *testing.T, h *fakeHub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("HF_ENDPOINT", srv.URL)
	return srv
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/models/org/model/revision/"):
		json.NewEncoder(w).Encode(map[string]any{"sha": testCommit, "lastModified": "2024-02-19T10:57:45.000Z"})
	case r.URL.Path == "/api/models/org/model/tree/"+testCommit:
		var all []treeEntry
		for _, d := range h.dirs {
			all = append(all, treeEntry{Type: "directory", Path: d})
		}
		for path, data := range h.files {
			all = append(all, treeEntry{Type: "file", Path: path, Size: int64(len(data))})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Path < all[j].Path })
		// Two pages, the way the Hub pages a long listing.
		half := len(all) / 2
		page := all[:half]
		if r.URL.Query().Get("cursor") == "2" {
			page = all[half:]
		} else {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?recursive=true&cursor=2>; rel="next"`, r.Host, r.URL.Path))
		}
		json.NewEncoder(w).Encode(page)
	case strings.HasPrefix(r.URL.Path, "/org/model/resolve/"+testCommit+"/"):
		path := strings.TrimPrefix(r.URL.Path, "/org/model/resolve/"+testCommit+"/")
		h.mu.Lock()
		h.resolves++
		gen := h.resolves
		h.mu.Unlock()
		target := "/cdn/" + (&url.URL{Path: path}).EscapedPath() + fmt.Sprintf("?gen=%d", gen)
		if h.cdnExpiry != "" {
			target += "&Expires=" + h.cdnExpiry
		}
		http.Redirect(w, r, target, http.StatusFound)
	case strings.HasPrefix(r.URL.Path, "/cdn/"):
		path := strings.TrimPrefix(r.URL.Path, "/cdn/")
		h.mu.Lock()
		revoked := h.revoked
		h.mu.Unlock()
		if revoked {
			http.Error(w, "expired", http.StatusForbidden)
			return
		}
		data, ok := h.files[path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.mu.Lock()
		h.served = append(h.served, path)
		h.mu.Unlock()
		http.ServeContent(w, r, path, time.Time{}, bytes.NewReader(data))
	default:
		http.NotFound(w, r)
	}
}

func (h *fakeHub) reset() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.served
	h.served = nil
	return out
}

func randomBytes(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

func testRepo() *fakeHub {
	return &fakeHub{
		files: map[string][]byte{
			"config.json":         []byte(`{"model_type": "gpt2"}`),
			"model.safetensors":   randomBytes(1, 3<<20+123),
			"onnx/decoder.onnx":   randomBytes(2, 1<<20+7),
			"onnx/tokenizer.json": []byte(`{"version": "1.0"}`),
			"a/b/deep.txt":        []byte("deep\n"),
			"empty.txt":           nil,
			"name with space.md":  []byte("# hi\n"),
		},
		dirs: []string{"onnx", "a", "a/b"},
	}
}

// readImage reads a whole image through ReadRange in 1 MiB pieces, the way
// the block cache asks for it.
func readImage(t *testing.T, b *Backend, uri string) []byte {
	t.Helper()
	info, err := b.Stat(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	for off := int64(0); off < info.Size; off += 1 << 20 {
		n := min(int64(1<<20), info.Size-off)
		part, err := b.ReadRange(context.Background(), uri, off, n, "", "")
		if err != nil {
			t.Fatalf("read at %d: %v", off, err)
		}
		out = append(out, part...)
	}
	return out
}

// lookup walks a path from the root the way the kernel resolves it.
func lookup(t *testing.T, img erofstest.Image, path string) erofstest.Inode {
	t.Helper()
	nid := uint64(binary.LittleEndian.Uint16(img.Bytes[erofs.SuperOffset+14:]))
	in := img.Inode(nid)
	if path == "" {
		return in
	}
	for _, name := range strings.Split(path, "/") {
		found := false
		for _, e := range img.Readdir(in) {
			if e.Name == name {
				in, found = img.Inode(e.Nid), true
				break
			}
		}
		if !found {
			t.Fatalf("%s: no entry %q", path, name)
		}
	}
	return in
}

func TestImageHoldsEveryFile(t *testing.T) {
	hub := testRepo()
	newFakeHub(t, hub)
	b := New()
	uri := "hf://org/model"

	info, err := b.Stat(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}
	if info.ETag != layoutVersion+testCommit {
		t.Errorf("ETag %q, want the commit %q", info.ETag, testCommit)
	}
	img := erofstest.Image{T: t, Bytes: readImage(t, b, uri)}
	for path, want := range hub.files {
		in := lookup(t, img, path)
		if in.Mode&0o170000 != 0o100000 {
			t.Errorf("%s: mode %o, want a regular file", path, in.Mode)
		}
		if in.Size != int64(len(want)) {
			t.Fatalf("%s: size %d, want %d", path, in.Size, len(want))
		}
		if len(want) > 0 && !bytes.Equal(img.Data(in), want) {
			t.Errorf("%s: data differs", path)
		}
		if in.Mtime != time.Date(2024, 2, 19, 10, 57, 45, 0, time.UTC).Unix() {
			t.Errorf("%s: mtime %d, want the commit's time", path, in.Mtime)
		}
	}
	var names []string
	for _, e := range img.Readdir(lookup(t, img, "onnx")) {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, " "); got != ". .. decoder.onnx tokenizer.json" {
		t.Errorf("onnx lists %q", got)
	}
	if root := lookup(t, img, ""); root.Nlink != 4 {
		t.Errorf("root nlink %d, want 4 (itself, its parent, onnx, a)", root.Nlink)
	}
}

func TestReadTouchesOnlyTheFilesItCovers(t *testing.T) {
	hub := testRepo()
	newFakeHub(t, hub)
	b := New()
	uri := "hf://org/model"
	info, err := b.Stat(context.Background(), uri)
	if err != nil {
		t.Fatal(err)
	}

	// The metadata alone: no file is fetched.
	img := b.repos[uri]
	if _, err := b.ReadRange(context.Background(), uri, 0, img.Extents[0].Start, "", ""); err != nil {
		t.Fatal(err)
	}
	if served := hub.reset(); len(served) != 0 {
		t.Errorf("reading metadata fetched %v", served)
	}

	// One block inside the large file: only that file.
	var big virtual.Extent
	for _, e := range img.Extents {
		if e.Size == int64(len(hub.files["model.safetensors"])) {
			big = e
		}
	}
	if big.Start%align != 0 {
		t.Errorf("a file over 1 MiB starts at %d, not on a 1 MiB boundary", big.Start)
	}
	got, err := b.ReadRange(context.Background(), uri, big.Start+(1<<20), 1<<20, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, hub.files["model.safetensors"][1<<20:2<<20]) {
		t.Error("the block's bytes differ from the file's")
	}
	if served := hub.reset(); len(served) != 1 || served[0] != "model.safetensors" {
		t.Errorf("one block of model.safetensors fetched %v", served)
	}

	// The tail block of the image, past the last file's end, reads as zeros
	// beyond the file.
	if _, err := b.ReadRange(context.Background(), uri, info.Size-erofs.BlockSize, erofs.BlockSize, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadRange(context.Background(), uri, info.Size-10, 11, "", ""); err == nil {
		t.Error("a read past the end of the image succeeded")
	}
}

func TestAFileThatChangedSizeFailsTheRead(t *testing.T) {
	hub := testRepo()
	newFakeHub(t, hub)
	b := New()
	uri := "hf://org/model"
	if _, err := b.Stat(context.Background(), uri); err != nil {
		t.Fatal(err)
	}
	hub.files["model.safetensors"] = randomBytes(3, 2<<20)
	var big virtual.Extent
	for _, e := range b.repos[uri].Extents {
		if e.Size == 3<<20+123 {
			big = e
		}
	}
	_, err := b.ReadRange(context.Background(), uri, big.Start, 4096, "", "")
	if !errors.Is(err, object.ErrChanged) {
		t.Fatalf("got %v, want ErrChanged", err)
	}
	if object.Retryable(err) {
		t.Error("a changed file is retryable")
	}
}

func TestMissingRepositoryIsPermanent(t *testing.T) {
	newFakeHub(t, testRepo())
	_, err := New().Stat(context.Background(), "hf://org/missing")
	if err == nil || object.Retryable(err) {
		t.Fatalf("got %v, want a permanent error", err)
	}
}

func TestParseURI(t *testing.T) {
	for _, tc := range []struct {
		uri, kind, id, rev, web string
	}{
		{"hf://openai-community/gpt2", "models", "openai-community/gpt2", "main", "/openai-community/gpt2"},
		{"hf://org/m.v1@abc123", "models", "org/m.v1", "abc123", "/org/m.v1"},
		{"hf://datasets/org/data@refs/pr/1", "datasets", "org/data", "refs/pr/1", "/datasets/org/data"},
		{"hf://spaces/org/app", "spaces", "org/app", "main", "/spaces/org/app"},
	} {
		ref, err := parseURI(tc.uri)
		if err != nil {
			t.Errorf("%s: %v", tc.uri, err)
			continue
		}
		if ref.kind != tc.kind || ref.id != tc.id || ref.revision != tc.rev || ref.webPrefix() != tc.web {
			t.Errorf("%s: got %+v %s", tc.uri, ref, ref.webPrefix())
		}
	}
	for _, bad := range []string{"hf://gpt2", "hf://a/b/c", "hf://org/model@", "hf://../x", "s3://a/b"} {
		if _, err := parseURI(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}

func TestAPathListedTwiceIsAnError(t *testing.T) {
	hub := testRepo()
	hub.dirs = append(hub.dirs, "onnx")
	newFakeHub(t, hub)
	if _, err := New().Stat(context.Background(), "hf://org/model"); err == nil {
		t.Fatal("a listing with onnx twice built an image")
	}
}

func TestTheCDNLocationIsRememberedUntilItStopsWorking(t *testing.T) {
	hub := testRepo()
	hub.cdnExpiry = fmt.Sprint(time.Now().Add(time.Hour).Unix())
	newFakeHub(t, hub)
	b := New()
	uri := "hf://org/model"
	if _, err := b.Stat(context.Background(), uri); err != nil {
		t.Fatal(err)
	}
	var big virtual.Extent
	for _, e := range b.repos[uri].Extents {
		if e.Size == 3<<20+123 {
			big = e
		}
	}
	for i := int64(0); i < 3; i++ {
		if _, err := b.ReadRange(context.Background(), uri, big.Start+i<<20, 1<<20, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if hub.resolves != 1 {
		t.Errorf("three reads of one file went through the Hub %d times, want once", hub.resolves)
	}

	// The CDN stops honouring the URL: the read goes back through the Hub.
	hub.mu.Lock()
	hub.revoked = true
	hub.mu.Unlock()
	if _, err := b.ReadRange(context.Background(), uri, big.Start, 4096, "", ""); err == nil {
		t.Fatal("a read succeeded while the CDN refused everything")
	}
	if hub.resolves != 2 {
		t.Errorf("after the CDN refused a remembered URL, resolves = %d, want 2", hub.resolves)
	}
	hub.mu.Lock()
	hub.revoked = false
	hub.mu.Unlock()
	got, err := b.ReadRange(context.Background(), uri, big.Start, 4096, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, hub.files["model.safetensors"][:4096]) {
		t.Error("bytes differ after going back through the Hub")
	}
}

func TestAnExpiredLocationIsNotUsed(t *testing.T) {
	hub := testRepo()
	hub.cdnExpiry = fmt.Sprint(time.Now().Add(30 * time.Second).Unix()) // under the one-minute margin
	newFakeHub(t, hub)
	b := New()
	uri := "hf://org/model"
	if _, err := b.Stat(context.Background(), uri); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := b.ReadRange(context.Background(), uri, b.repos[uri].Extents[0].Start, 10, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if hub.resolves != 2 {
		t.Errorf("a URL about to expire was reused: resolves = %d, want 2", hub.resolves)
	}
}
