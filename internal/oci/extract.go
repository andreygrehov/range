package oci

import (
	"archive/tar"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// extractLayer unpacks one image layer, honouring the whiteout markers that
// record deletions from lower layers.
//
// Every path operation goes through os.Root, which refuses to resolve a
// component that leaves the directory. That is what stops a layer shipping
// "x -> /etc" followed by a regular file "x/passwd", or hardlinking a host
// file into the image: both are ordinary-looking tar entries whose damage is
// done by the symlink underneath them, not by ".." in the name.
func extractLayer(source io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	// touched holds every path this layer wrote and every directory above one,
	// so a whiteout removes only what lower layers put there, whichever order
	// the tar lists the entries in.
	touched := map[string]bool{}
	reader := tar.NewReader(source)
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
		parent, base := path.Dir(name), path.Base(name)
		if strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				// An opaque marker hides everything the lower layers put
				// under this directory, at any depth, so that goes rather
				// than the marker.
				pruneExtracted(root, parent, touched)
				continue
			}
			if hidden := path.Join(parent, strings.TrimPrefix(base, ".wh.")); !touched[hidden] {
				root.RemoveAll(hidden)
			}
			continue
		}
		for p := name; p != "." && !touched[p]; p = path.Dir(p) {
			touched[p] = true
		}
		mode := header.FileInfo().Mode()
		if parent != "." {
			if err := root.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, mode.Perm()); err != nil {
				return err
			}
		case tar.TypeReg:
			root.Remove(name)
			file, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode.Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(file, reader); err != nil {
				file.Close()
				return err
			}
			file.Close()
		case tar.TypeSymlink:
			root.Remove(name)
			if err := root.Symlink(header.Linkname, name); err != nil {
				return err
			}
		case tar.TypeLink:
			root.Remove(name)
			target := strings.TrimPrefix(path.Clean("/"+filepath.ToSlash(header.Linkname)), "/")
			if err := root.Link(target, name); err != nil {
				return err
			}
		default:
			continue // devices and fifos are not needed for an environment image
		}
		if header.Typeflag == tar.TypeSymlink {
			root.Lchown(name, header.Uid, header.Gid)
			continue
		}
		// Chown clears setuid and setgid, so the mode is applied after it or
		// every sudo in the image comes out unprivileged.
		root.Chown(name, header.Uid, header.Gid)
		root.Chmod(name, mode)
		root.Chtimes(name, header.ModTime, header.ModTime)
	}
}

// pruneExtracted is the opaque whiteout on disk: under dir it removes every
// entry the current layer did not touch, and descends into those it did.
func pruneExtracted(root *os.Root, dir string, touched map[string]bool) {
	entries, err := fs.ReadDir(root.FS(), dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		child := path.Join(dir, entry.Name())
		switch {
		case !touched[child]:
			root.RemoveAll(child)
		case entry.IsDir():
			pruneExtracted(root, child, touched)
		}
	}
}
