package oci

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// An image's commands follow from its layer indexes alone: each index lists
// every file of its layer, with its mode and link. Commands reads no layer,
// so the catalog lists the commands of thousands of images in minutes, and
// the command map says which image holds pdflatex.

// ImageCommands is what one image provides.
type ImageCommands struct {
	Image    string `json:"image"`
	Platform string `json:"platform"`
	// Layers are the image's layer digests, bottom first.
	Layers []string `json:"layers"`
	// Commands maps each program on the image's PATH to the index, in
	// Layers, of the layer its file came from.
	Commands map[string]int `json:"commands"`
	// Entrypoint is the program the image runs, when its entrypoint names
	// one by path. An image built from scratch can hold its program outside
	// PATH, as /jq: this names it.
	Entrypoint string `json:"entrypoint,omitempty"`
	// EntrypointPath is where the entrypoint's program is: /jq.
	EntrypointPath string `json:"entrypoint_path,omitempty"`
	// OS lists the commands the image's operating system already has at the
	// same path. A later layer that updates one, as apt-get upgrade does,
	// does not make it the image's own.
	OS []string `json:"os,omitempty"`
}

// ErrNotIndexed says that a layer of the image has no index in the catalog.
var ErrNotIndexed = errors.New("a layer of the image is not in the catalog")

// defaultPath is PATH in an image that sets none.
const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// Commands lists the commands of image for platform from its layer indexes:
// those in dir, then the catalog's. It builds no index.
func Commands(ctx context.Context, image string, platform Platform, dir string) (ImageCommands, error) {
	r, err := resolve(ctx, image, platform)
	if err != nil {
		return ImageCommands{}, err
	}
	l := NewLazy(image, platform, dir)
	l.fetchCatalogIndexes(ctx, r)
	var entries [][]Entry
	for _, layer := range r.manifest.Layers {
		idx, err := loadIndex(l.indexPath(layer.Digest))
		if err != nil || idx.Digest != layer.Digest {
			return ImageCommands{}, fmt.Errorf("%s: %s: %w", image, layer.Digest, ErrNotIndexed)
		}
		entries = append(entries, idx.Entries)
	}
	c := commandsOf(entries, r.config)
	c.Image, c.Platform = image, platform.String()
	for _, layer := range r.manifest.Layers {
		c.Layers = append(c.Layers, layer.Digest)
	}
	return c, nil
}

// commandsOf finds the commands in the layers, bottom first, of an image with
// config.
func commandsOf(layers [][]Entry, config ImageConfig) ImageCommands {
	fs := mergeLayers(layers)
	c := ImageCommands{Commands: map[string]int{}}
	add := func(name, p string) {
		if _, seen := c.Commands[name]; seen {
			return
		}
		if f, ok := fs.executable(p); ok {
			c.Commands[name] = f.layer
			if fs.inOS(p) {
				c.OS = append(c.OS, name)
			}
		}
	}
	if ep := config.Config.Entrypoint; len(ep) > 0 && strings.HasPrefix(ep[0], "/") {
		if _, ok := fs.executable(cleanPath(ep[0])); ok {
			c.Entrypoint, c.EntrypointPath = path.Base(ep[0]), path.Clean(ep[0])
			add(c.Entrypoint, cleanPath(ep[0]))
		}
	}
	search := defaultPath
	for _, kv := range config.Config.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			search = v
		}
	}
	// PATH order decides which file a name runs, as in a shell.
	for _, dir := range strings.Split(search, ":") {
		if dir == "" {
			continue
		}
		for _, p := range fs.list(fs.dir(cleanPath(dir))) {
			add(path.Base(p), p)
		}
	}
	return c
}

// layerFile is a file of a merged image, and the layer it came from.
type layerFile struct {
	entry Entry
	layer int
}

// mergedFS is an image's files after its layers are stacked, whiteouts
// applied.
type mergedFS struct {
	files map[string]layerFile
	// dirs lists the names in each directory.
	dirs map[string][]string
	// osFiles are the paths of the OS layer: the lowest one with an
	// os-release. An image built from scratch has none.
	osFiles map[string]bool
}

func mergeLayers(layers [][]Entry) *mergedFS {
	fs := &mergedFS{files: map[string]layerFile{}}
	for i, entries := range layers {
		// A whiteout hides what lower layers put there; an opaque directory
		// hides everything lower layers put in it.
		gone, opaque := map[string]bool{}, map[string]bool{}
		for _, e := range entries {
			dir, base := path.Split(cleanPath(e.Name))
			switch {
			case base == ".wh..wh..opq":
				opaque[dirKey(dir)] = true
			case strings.HasPrefix(base, ".wh."):
				gone[dir+strings.TrimPrefix(base, ".wh.")] = true
			}
		}
		if len(gone)+len(opaque) > 0 {
			for p := range fs.files {
				if hidden(p, gone, opaque) {
					delete(fs.files, p)
				}
			}
		}
		for _, e := range entries {
			if p := cleanPath(e.Name); !strings.HasPrefix(path.Base(p), ".wh.") {
				fs.files[p] = layerFile{e, i}
			}
		}
		if fs.osFiles == nil && hasOSRelease(entries) {
			fs.osFiles = map[string]bool{}
			for _, e := range entries {
				fs.osFiles[cleanPath(e.Name)] = true
			}
		}
	}
	fs.dirs = map[string][]string{}
	for p := range fs.files {
		if p != "" {
			dir := path.Dir(p)
			fs.dirs[dir] = append(fs.dirs[dir], p)
		}
	}
	return fs
}

// dirKey is a directory as path.Dir names it: the root is ".".
func dirKey(dir string) string {
	if dir = strings.TrimSuffix(dir, "/"); dir == "" {
		return "."
	}
	return dir
}

// hidden reports whether a higher layer hides p: a whiteout of p or of a
// directory above it, or an opaque directory above it.
func hidden(p string, gone, opaque map[string]bool) bool {
	if gone[p] {
		return true
	}
	for dir := path.Dir(p); ; dir = path.Dir(dir) {
		if gone[dir] || opaque[dir] {
			return true
		}
		if dir == "." {
			return false
		}
	}
}

func hasOSRelease(entries []Entry) bool {
	for _, e := range entries {
		if p := cleanPath(e.Name); p == "etc/os-release" || p == "usr/lib/os-release" {
			return true
		}
	}
	return false
}

// list returns the paths in dir, sorted so the result is stable.
func (fs *mergedFS) list(dir string) []string {
	paths := append([]string(nil), fs.dirs[dirKey(dir)]...)
	sort.Strings(paths)
	return paths
}

// dir resolves a directory, its last element too: /bin is usr/bin when bin
// links to usr/bin, and /usr/local/cuda/bin is usr/local/cuda-12.8/bin.
func (fs *mergedFS) dir(p string) string {
	p = fs.through(p)
	for hops := 0; hops < 40; hops++ {
		f, ok := fs.files[p]
		if !ok || f.entry.Typeflag != '2' {
			break
		}
		p = fs.through(linkTarget(p, f.entry.Linkname))
	}
	return p
}

// through follows the links among the directories of p, as the kernel does:
// bin/sh is usr/bin/sh when bin links to usr/bin. The last element stays.
func (fs *mergedFS) through(p string) string {
	parts := strings.Split(p, "/")
	cur := ""
	for i, part := range parts {
		next := path.Join(cur, part)
		for hops := 0; hops < 40 && i < len(parts)-1; hops++ {
			f, ok := fs.files[next]
			if !ok || f.entry.Typeflag != '2' {
				break
			}
			next = linkTarget(next, f.entry.Linkname)
		}
		cur = next
	}
	return cur
}

// executable returns the regular file that p leads to, when it may run.
func (fs *mergedFS) executable(p string) (layerFile, bool) {
	for hops := 0; hops < 40; hops++ {
		f, ok := fs.files[p]
		if !ok {
			if q := fs.through(p); q != p {
				p = q
				continue
			}
			return layerFile{}, false
		}
		switch f.entry.Typeflag {
		case '0', 0, '7':
			return f, f.entry.Mode&0o111 != 0
		case '1':
			p = cleanPath(f.entry.Linkname)
		case '2':
			p = linkTarget(p, f.entry.Linkname)
		default:
			return layerFile{}, false
		}
	}
	return layerFile{}, false
}

// inOS reports whether the OS layer has p, under its own name or the one a
// link among its directories gives it.
func (fs *mergedFS) inOS(p string) bool {
	return fs.osFiles[p] || fs.osFiles[fs.through(p)]
}

// linkTarget resolves a symbolic link at p to target, inside the image.
func linkTarget(p, target string) string {
	if !strings.HasPrefix(target, "/") {
		target = path.Join(path.Dir(p), target)
	}
	return cleanPath(target)
}

// cleanPath is a path inside an image without its leading slash: "usr/bin".
// The root is "".
func cleanPath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}
