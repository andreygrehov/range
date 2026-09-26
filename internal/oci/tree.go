package oci

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/erofs"
)

// tree assembles an image's filesystem from its layers without writing
// anything to the local filesystem by path. Ownership, modes, setuid bits and
// times come from the tar headers, so they are the image's own whoever runs the
// build - a build without root cannot quietly hand /etc/shadow to the builder.
// File bodies go into a store named by their SHA-256, so a case-insensitive
// filesystem cannot fold xt_CONNMARK.h and xt_connmark.h into one file, and a
// layer has nowhere outside the store to write.
type tree struct {
	root  *erofs.Node
	blobs string
}

var epoch = time.Unix(0, 0)

func newDir(perm, uid, gid uint32, mtime time.Time) *erofs.Node {
	return &erofs.Node{Mode: syscall.S_IFDIR | perm, Uid: uid, Gid: gid, Mtime: mtime,
		Children: map[string]*erofs.Node{}, ChildLayer: map[string]int{}}
}

func newTree(blobs string) *tree {
	root := newDir(0o755, 0, 0, epoch)
	root.Parent = root
	return &tree{root: root, blobs: blobs}
}

// walkDir resolves a directory path inside the tree. Symlinks in the path are
// followed the way os.Root follows them - relative ones from where they sit,
// absolute ones from the image root, and never above it. With create, missing
// directories are made root-owned and 0755, and a non-directory in the way is
// replaced, as unpacking the layer onto disk would do.
func (t *tree) walkDir(p string, create bool, layer int) (*erofs.Node, error) {
	parts := strings.Split(p, "/")
	stack := []*erofs.Node{t.root}
	hops := 0
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		current := stack[len(stack)-1]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		child, ok := current.Children[part]
		if ok && child.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			if hops++; hops > 40 {
				return nil, fmt.Errorf("too many symlinks resolving %s", p)
			}
			if strings.HasPrefix(child.Target, "/") {
				stack = stack[:1]
			}
			parts = append(strings.Split(child.Target, "/"), parts...)
			continue
		}
		if !ok || child.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			if !create {
				return nil, os.ErrNotExist
			}
			child = newDir(0o755, 0, 0, epoch)
			current.Children[part] = child
			current.ChildLayer[part] = layer
		}
		stack = append(stack, child)
	}
	return stack[len(stack)-1], nil
}

// lookup finds the node at p, following a final symlink when follow is set.
func (t *tree) lookup(p string, follow bool) (*erofs.Node, error) {
	for hops := 0; hops < 40; hops++ {
		dir, err := t.walkDir(path.Dir(p), false, 0)
		if err != nil {
			return nil, err
		}
		node, ok := dir.Children[path.Base(p)]
		if !ok {
			return nil, os.ErrNotExist
		}
		if !follow || node.Mode&syscall.S_IFMT != syscall.S_IFLNK {
			return node, nil
		}
		if strings.HasPrefix(node.Target, "/") {
			p = strings.TrimPrefix(path.Clean(node.Target), "/")
		} else {
			p = path.Join(path.Dir(p), node.Target)
		}
	}
	return nil, fmt.Errorf("too many symlinks resolving %s", p)
}

// storeBlob writes one file body into the content store and returns its path.
func (t *tree) storeBlob(r io.Reader, size int64) (string, error) {
	temp, err := os.CreateTemp(t.blobs, ".partial-*")
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(temp, sum), r)
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil && written != size {
		err = fmt.Errorf("file body is %d bytes, header says %d", written, size)
	}
	if err != nil {
		os.Remove(temp.Name())
		return "", err
	}
	name := filepath.Join(t.blobs, hex.EncodeToString(sum.Sum(nil)))
	if _, err := os.Stat(name); err == nil {
		os.Remove(temp.Name()) // the same body was already stored
		return name, nil
	}
	return name, os.Rename(temp.Name(), name)
}

// applyLayer applies one layer's uncompressed tar stream to the tree,
// whiteouts included.
func (t *tree) applyLayer(body io.Reader, layer int) error {
	reader := tar.NewReader(body)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(header.Name)), "/")
		if name == "" || name == "." {
			continue
		}
		base := path.Base(name)
		if len(base) > erofs.NameMax {
			return fmt.Errorf("name longer than %d bytes: %s", erofs.NameMax, name)
		}
		parent, err := t.walkDir(path.Dir(name), true, layer)
		if err != nil {
			return err
		}
		if strings.HasPrefix(base, ".wh.") {
			// A whiteout hides what lower layers put there; entries from
			// this same layer stay, whichever order the tar lists them in.
			if base == ".wh..wh..opq" {
				pruneLowerLayers(parent, layer)
			} else if hidden := strings.TrimPrefix(base, ".wh."); parent.ChildLayer[hidden] < layer {
				delete(parent.Children, hidden)
				delete(parent.ChildLayer, hidden)
			}
			continue
		}
		perm := uint32(header.Mode) & 0o7777
		uid, gid := uint32(header.Uid), uint32(header.Gid)
		var node *erofs.Node
		switch header.Typeflag {
		case tar.TypeDir:
			if existing, ok := parent.Children[base]; ok && existing.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				existing.Mode = syscall.S_IFDIR | perm
				existing.Uid, existing.Gid, existing.Mtime = uid, gid, header.ModTime
				parent.ChildLayer[base] = layer
				continue
			}
			node = newDir(perm, uid, gid, header.ModTime)
		case tar.TypeReg, '\x00':
			node = &erofs.Node{Mode: syscall.S_IFREG | perm, Uid: uid, Gid: gid,
				Mtime: header.ModTime, Size: header.Size}
			if header.Size > 0 {
				if node.Source, err = t.storeBlob(reader, header.Size); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			}
		case tar.TypeSymlink:
			if len(header.Linkname) >= erofs.BlockSize {
				return fmt.Errorf("symlink %s is too long", name)
			}
			node = &erofs.Node{Mode: syscall.S_IFLNK | 0o777, Uid: uid, Gid: gid,
				Mtime: header.ModTime, Target: header.Linkname, Size: int64(len(header.Linkname))}
		case tar.TypeLink:
			target := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(header.Linkname)), "/")
			existing, err := t.lookup(target, false)
			if err != nil {
				return fmt.Errorf("hardlink %s to %s: %w", name, target, err)
			}
			if existing.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				return fmt.Errorf("hardlink %s points at a directory", name)
			}
			node = existing
		case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
			kind := map[byte]uint32{tar.TypeChar: syscall.S_IFCHR, tar.TypeBlock: syscall.S_IFBLK,
				tar.TypeFifo: syscall.S_IFIFO}[header.Typeflag]
			node = &erofs.Node{Mode: kind | perm, Uid: uid, Gid: gid, Mtime: header.ModTime}
			if header.Typeflag != tar.TypeFifo {
				major, minor := uint32(header.Devmajor), uint32(header.Devminor)
				node.Rdev = (minor & 0xff) | (major << 8) | ((minor &^ 0xff) << 12)
			}
		default:
			continue
		}
		parent.Children[base] = node
		parent.ChildLayer[base] = layer
	}
}

// pruneLowerLayers is an opaque whiteout: everything under dir that a lower
// layer put there goes, at any depth, and everything this layer wrote stays. A
// directory survives if this layer declared it or wrote anything inside it,
// which is what containerd does when a layer lists a subdirectory before the
// marker. It reports whether dir still holds anything from this layer.
func pruneLowerLayers(dir *erofs.Node, layer int) bool {
	kept := false
	for name, child := range dir.Children {
		mine := dir.ChildLayer[name] >= layer
		if child.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			mine = pruneLowerLayers(child, layer) || mine
		}
		if !mine {
			delete(dir.Children, name)
			delete(dir.ChildLayer, name)
			continue
		}
		kept = true
	}
	return kept
}

// addFile places a file Range itself contributes, owned by root.
func (t *tree) addFile(p string, body []byte, perm uint32) error {
	parent, err := t.walkDir(path.Dir(p), true, math.MaxInt32)
	if err != nil {
		return err
	}
	blob, err := t.storeBlob(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return err
	}
	parent.Children[path.Base(p)] = &erofs.Node{Mode: syscall.S_IFREG | perm, Mtime: epoch,
		Size: int64(len(body)), Source: blob}
	parent.ChildLayer[path.Base(p)] = math.MaxInt32
	return nil
}

// nodes flattens the tree for the writer: every inode once, root first, in
// name order, with parents, entries and link counts filled in.
func (t *tree) nodes() (*erofs.Node, []*erofs.Node) {
	var out []*erofs.Node
	refs := map[*erofs.Node]uint32{}
	var visit func(dir *erofs.Node)
	visit = func(dir *erofs.Node) {
		out = append(out, dir)
		names := make([]string, 0, len(dir.Children))
		for name := range dir.Children {
			names = append(names, name)
		}
		sort.Strings(names)
		dir.Entries = dir.Entries[:0]
		subdirs := 0
		for _, name := range names {
			child := dir.Children[name]
			dir.Entries = append(dir.Entries, erofs.Dirent{Name: name, Node: child})
			if child.Mode&syscall.S_IFMT == syscall.S_IFDIR {
				subdirs++
				child.Parent = dir
				visit(child)
				continue
			}
			if refs[child]++; refs[child] == 1 {
				out = append(out, child)
			}
		}
		dir.Nlink = uint32(2 + subdirs)
	}
	t.root.Parent = t.root
	visit(t.root)
	for node, count := range refs {
		node.Nlink = count
	}
	return t.root, out
}

// BuildEROFS turns a container image straight into an EROFS image. Nothing
// is unpacked onto the local filesystem, so it needs no root and no Linux, and
// runs the same on a Mac as on a build server.
func BuildEROFS(ctx context.Context, image, output string, align int64, platform Platform) (erofs.Stats, error) {
	blobs, err := os.MkdirTemp("", "range-blobs-")
	if err != nil {
		return erofs.Stats{}, err
	}
	defer os.RemoveAll(blobs)
	tree := newTree(blobs)
	fmt.Fprintf(os.Stderr, "Pulling %s for %s\n", image, platform)
	config, err := fetch(ctx, image, platform, tree.applyLayer)
	if err != nil {
		return erofs.Stats{}, fmt.Errorf("pull %s: %w", image, err)
	}
	// The mount points a Linux environment expects, and a /tmp anyone can use.
	for _, dir := range []string{"proc", "sys", "dev", "etc"} {
		if _, err := tree.walkDir(dir, true, math.MaxInt32); err != nil {
			return erofs.Stats{}, err
		}
	}
	tmp, err := tree.walkDir("tmp", true, math.MaxInt32)
	if err != nil {
		return erofs.Stats{}, err
	}
	tmp.Mode = syscall.S_IFDIR | 0o1777
	meta := environmentFor(image, config, platform, func(p string) bool {
		node, err := tree.lookup(p, true)
		return err == nil && node.Mode&syscall.S_IFMT == syscall.S_IFREG
	})
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return erofs.Stats{}, err
	}
	if err := tree.addFile("etc/range/environment.json", body, 0o644); err != nil {
		return erofs.Stats{}, err
	}
	root, nodes := tree.nodes()
	return erofs.WriteNodes(root, nodes, output, align)
}
