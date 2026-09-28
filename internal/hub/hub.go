// Package hub serves a Hugging Face repository as a read-only EROFS image
// without copying it.
//
// Opening hf://org/name lists the repository at one commit and writes only the
// image's metadata: superblock, inodes and directories, a few kilobytes held in
// memory. Each file's data region maps to that file on the Hub, so a read of
// the image becomes a ranged GET of the files it covers. The commit is pinned
// when the repository is opened, and every URL names it, so a push to the
// repository never changes an open image.
package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/virtual"
)

// Scheme prefixes every Hub URI: hf://org/name, hf://datasets/org/name or
// hf://spaces/org/name, each with an optional @revision.
const Scheme = "hf://"

// DefaultEndpoint is the Hub. HF_ENDPOINT overrides it, as it does for the
// Hub's own clients.
const DefaultEndpoint = "https://huggingface.co"

// layoutVersion is part of the identity the block cache keys on. The image
// is laid out by this code, not stored anywhere, so a change to the layout
// must change the key, or cached blocks of the old layout would be served.
const layoutVersion = "range-hub-1:"

// align places every file of at least this size on its own 1 MiB boundary, so
// one cache block of a large file never spans two files and costs two requests.
const align = 1 << 20

// fetchConcurrency bounds the ranged GETs one read issues when it covers
// several small files.
const fetchConcurrency = 8

const userAgent = "range (+https://getrange.sh)"

// IsURI reports whether uri names a Hub repository.
func IsURI(uri string) bool { return strings.HasPrefix(uri, Scheme) }

// Backend implements object.Backend for hf:// URIs.
type Backend struct {
	client   *http.Client
	endpoint string

	mu    sync.Mutex
	repos map[string]*image

	// The Hub redirects every file to a signed CDN URL that stays valid for
	// hours. Remembering it saves one round trip on every later read.
	locMu     sync.Mutex
	locations map[string]location
}

// location is a file's CDN URL and when to stop using it.
type location struct {
	url   string
	until time.Time
}

// maxRedirects bounds the hops from a resolve URL to the bytes.
const maxRedirects = 5

// New returns a Backend for the Hub at HF_ENDPOINT, or the public Hub.
func New() *Backend {
	endpoint := strings.TrimRight(os.Getenv("HF_ENDPOINT"), "/")
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	return &Backend{
		client: &http.Client{
			Timeout: 60 * time.Second,
			// fetch follows redirects itself, to remember where they lead.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		endpoint:  endpoint,
		repos:     map[string]*image{},
		locations: map[string]location{},
	}
}

// image is one repository at one commit, laid out as EROFS.
type image struct {
	commit   string
	modified time.Time
	*virtual.Image
}

// file is one file of the repository: where to fetch it and how big it is.
type file struct {
	url  string
	size int64
}

// Stat lists the repository on first use and reports the image it becomes.
// The ETag is the commit, so the cache key changes when the commit does.
func (b *Backend) Stat(ctx context.Context, uri string) (object.Info, error) {
	img, err := b.open(ctx, uri)
	if err != nil {
		return object.Info{}, err
	}
	return object.Info{Size: img.Size, ETag: layoutVersion + img.commit, LastModified: img.modified}, nil
}

// ReadRange assembles a range of the image: metadata from memory, file bytes
// from the Hub, and zeros in the padding between files.
func (b *Backend) ReadRange(ctx context.Context, uri string, offset, length int64, _, _ string) ([]byte, error) {
	img, err := b.open(ctx, uri)
	if err != nil {
		return nil, err
	}
	return img.Image.ReadRange(ctx, offset, length)
}

// statusError carries an HTTP status where object.Retryable can see it.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string       { return e.msg }
func (e *statusError) HTTPStatusCode() int { return e.code }

// fetch fills dst with a file's bytes from offset on. The Hub answers a
// resolve URL with a redirect to its CDN; fetch follows it and remembers the
// CDN URL until it expires. A remembered URL that stops working is forgotten
// and the read goes back through the Hub once. The Content-Range total is
// checked against the listed size so a wrong file can never fill the range.
func (b *Backend) fetch(ctx context.Context, ext file, offset int64, dst []byte) error {
	if cdn, ok := b.location(ext.url); ok {
		err := b.fetchFrom(ctx, cdn, ext, offset, dst)
		var status *statusError
		if err == nil || !errors.As(err, &status) {
			return err
		}
		b.forget(ext.url) // expired or revoked: ask the Hub again
	}
	return b.fetchFrom(ctx, ext.url, ext, offset, dst)
}

func (b *Backend) fetchFrom(ctx context.Context, target string, ext file, offset int64, dst []byte) error {
	var resp *http.Response
	for hops := 0; ; hops++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+int64(len(dst))-1))
		req.Header.Set("User-Agent", userAgent)
		resp, err = b.client.Do(req)
		if err != nil {
			return fmt.Errorf("get %s: %w", target, err)
		}
		if resp.StatusCode < 300 || resp.StatusCode >= 400 {
			break
		}
		next, err := resp.Location()
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("get %s: redirect without a location: %w", target, err)
		}
		if hops == maxRedirects {
			return fmt.Errorf("get %s: more than %d redirects", ext.url, maxRedirects)
		}
		target = next.String()
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return &statusError{resp.StatusCode, fmt.Sprintf("get %s: want 206, got %s", target, resp.Status)}
	}
	if total := contentRangeTotal(resp.Header.Get("Content-Range")); total != ext.size {
		return fmt.Errorf("%w: %s is %d bytes, the listing said %d", object.ErrChanged, ext.url, total, ext.size)
	}
	if _, err := io.ReadFull(resp.Body, dst); err != nil {
		return fmt.Errorf("get %s: short read: %w", ext.url, err)
	}
	if target != ext.url {
		b.remember(ext.url, target)
	}
	return nil
}

func (b *Backend) location(resolve string) (string, bool) {
	b.locMu.Lock()
	defer b.locMu.Unlock()
	loc, ok := b.locations[resolve]
	if !ok || time.Now().After(loc.until) {
		return "", false
	}
	return loc.url, true
}

// remember keeps a CDN URL until a minute before the expiry it was signed
// with, or for five minutes when it names none.
func (b *Backend) remember(resolve, cdn string) {
	until := time.Now().Add(5 * time.Minute)
	if u, err := url.Parse(cdn); err == nil {
		if secs, err := strconv.ParseInt(u.Query().Get("Expires"), 10, 64); err == nil {
			until = time.Unix(secs, 0).Add(-time.Minute)
		}
	}
	b.locMu.Lock()
	b.locations[resolve] = location{url: cdn, until: until}
	b.locMu.Unlock()
}

func (b *Backend) forget(resolve string) {
	b.locMu.Lock()
	delete(b.locations, resolve)
	b.locMu.Unlock()
}

// contentRangeTotal returns the total in "bytes a-b/total", or -1.
func contentRangeTotal(value string) int64 {
	slash := strings.LastIndexByte(value, '/')
	if slash < 0 {
		return -1
	}
	total, err := strconv.ParseInt(value[slash+1:], 10, 64)
	if err != nil {
		return -1
	}
	return total
}

// repoRef is a parsed hf:// URI.
type repoRef struct {
	kind     string // models, datasets or spaces: the API's word for it
	id       string // org/name
	revision string
}

var repoID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

func parseURI(uri string) (repoRef, error) {
	rest, ok := strings.CutPrefix(uri, Scheme)
	if !ok {
		return repoRef{}, fmt.Errorf("%s is not an %s URI", uri, Scheme)
	}
	ref := repoRef{kind: "models", revision: "main"}
	for _, kind := range []string{"datasets", "spaces"} {
		if after, ok := strings.CutPrefix(rest, kind+"/"); ok {
			ref.kind, rest = kind, after
		}
	}
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		rest, ref.revision = rest[:at], rest[at+1:]
	}
	if !repoID.MatchString(rest) || ref.revision == "" {
		return repoRef{}, fmt.Errorf("%s: want %sorg/name[@revision], or datasets/ or spaces/ before org", uri, Scheme)
	}
	ref.id = rest
	return ref, nil
}

// webPrefix is the repository's path on the website, where resolve URLs live.
func (r repoRef) webPrefix() string {
	if r.kind == "models" {
		return "/" + r.id
	}
	return "/" + r.kind + "/" + r.id
}

func (b *Backend) open(ctx context.Context, uri string) (*image, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if img, ok := b.repos[uri]; ok {
		return img, nil
	}
	ref, err := parseURI(uri)
	if err != nil {
		return nil, err
	}
	img, err := b.build(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", uri, err)
	}
	b.repos[uri] = img
	return img, nil
}

// treeEntry is one item of the Hub's tree listing.
type treeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// build pins the revision to a commit, lists every file at that commit and
// lays the listing out as an image whose file data stays on the Hub.
func (b *Backend) build(ctx context.Context, ref repoRef) (*image, error) {
	var rev struct {
		SHA          string    `json:"sha"`
		LastModified time.Time `json:"lastModified"`
	}
	api := b.endpoint + "/api/" + ref.kind + "/" + ref.id
	if err := b.getJSON(ctx, api+"/revision/"+url.PathEscape(ref.revision), &rev, nil); err != nil {
		return nil, err
	}
	if rev.SHA == "" {
		return nil, errors.New("the Hub did not name a commit for this revision")
	}

	var entries []treeEntry
	next := api + "/tree/" + rev.SHA + "?recursive=true"
	for next != "" {
		var page []treeEntry
		link := ""
		if err := b.getJSON(ctx, next, &page, &link); err != nil {
			return nil, err
		}
		entries = append(entries, page...)
		next = link
	}

	mtime := rev.LastModified
	root := &erofs.Node{Mode: syscall.S_IFDIR | 0o755, Mtime: mtime, Nlink: 2}
	root.Parent = root
	nodes := []*erofs.Node{root}
	dirs := map[string]*erofs.Node{"": root}
	var dirOf func(path string) (*erofs.Node, error)
	dirOf = func(path string) (*erofs.Node, error) {
		if dir, ok := dirs[path]; ok {
			return dir, nil
		}
		parentPath, name := splitPath(path)
		parent, err := dirOf(parentPath)
		if err != nil {
			return nil, err
		}
		if err := checkName(name); err != nil {
			return nil, err
		}
		dir := &erofs.Node{Mode: syscall.S_IFDIR | 0o755, Mtime: mtime, Nlink: 2, Parent: parent}
		parent.Entries = append(parent.Entries, erofs.Dirent{Name: name, Node: dir})
		parent.Nlink++
		dirs[path] = dir
		nodes = append(nodes, dir)
		return dir, nil
	}

	// The same commit must always give the same image, whatever order the
	// API lists it in.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	files := map[*erofs.Node]string{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if seen[entry.Path] {
			return nil, fmt.Errorf("the listing names %q twice", entry.Path)
		}
		seen[entry.Path] = true
		switch entry.Type {
		case "directory":
			if _, err := dirOf(entry.Path); err != nil {
				return nil, err
			}
		case "file":
			parentPath, name := splitPath(entry.Path)
			parent, err := dirOf(parentPath)
			if err != nil {
				return nil, err
			}
			if err := checkName(name); err != nil {
				return nil, err
			}
			file := &erofs.Node{
				Mode: syscall.S_IFREG | 0o644, Mtime: mtime, Nlink: 1,
				Size: entry.Size, Parent: parent, External: true,
			}
			parent.Entries = append(parent.Entries, erofs.Dirent{Name: name, Node: file})
			nodes = append(nodes, file)
			files[file] = entry.Path
		}
	}

	built, err := virtual.Build(root, nodes, align, func(node *erofs.Node) virtual.ReadFunc {
		f := file{
			url:  b.endpoint + ref.webPrefix() + "/resolve/" + rev.SHA + "/" + escapePath(files[node]),
			size: node.Size,
		}
		return func(ctx context.Context, dst []byte, off int64) error { return b.fetch(ctx, f, off, dst) }
	})
	if err != nil {
		return nil, err
	}
	built.Concurrency = fetchConcurrency
	return &image{commit: rev.SHA, modified: mtime, Image: built}, nil
}

func splitPath(path string) (dir, name string) {
	if slash := strings.LastIndexByte(path, '/'); slash >= 0 {
		return path[:slash], path[slash+1:]
	}
	return "", path
}

func checkName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > erofs.NameMax {
		return fmt.Errorf("the listing has a path element EROFS cannot hold: %q", name)
	}
	return nil
}

func escapePath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

var nextLink = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// getJSON decodes one API response. When link is set, it receives the URL of
// the next page, or "" on the last one.
func (b *Backend) getJSON(ctx context.Context, u string, into any, link *string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := b.client.Do(req)
	if err != nil {
		return fmt.Errorf("get %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("get %s: %s", u, resp.Status)
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			msg += " (private and gated repositories are not supported yet)"
		}
		return &statusError{resp.StatusCode, msg}
	}
	if link != nil {
		*link = ""
		if m := nextLink.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			*link = m[1]
		}
	}
	return json.NewDecoder(resp.Body).Decode(into)
}
