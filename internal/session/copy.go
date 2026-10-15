package session

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// range cp copies as docker cp does. The side that has the source sends it
// as a tar stream whose entries start with the source's base name, or with
// "." for its contents; the other side decides where it lands.

// CopySpec says what a copy carries.
type CopySpec struct {
	Base     string `json:"base"`               // the source's base name
	Dir      bool   `json:"dir,omitempty"`      // the source is a directory
	Contents bool   `json:"contents,omitempty"` // SRC/. : what is in it, not it
	Follow   bool   `json:"follow,omitempty"`   // -L: a link as SRC is what it points to
}

// CopyPlan is where a copy lands: in Dir, as Name, or, with Name "", its
// contents straight into Dir.
type CopyPlan struct{ Dir, Name string }

// PlanCopy decides where spec lands at dest, as docker cp does:
//
//   - a file goes into dest when dest is a directory, and is dest otherwise;
//   - a directory goes into dest when dest is one, or becomes dest when dest
//     does not exist;
//   - SRC/. puts the directory's contents in dest, made if it is not there.
func PlanCopy(spec CopySpec, dest string) (CopyPlan, error) {
	slash := strings.HasSuffix(dest, "/")
	dest = filepath.Clean(dest)
	info, err := os.Stat(dest)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return CopyPlan{}, err
	}
	isDir := exists && info.IsDir()
	if !spec.Dir {
		switch {
		case isDir:
			return CopyPlan{dest, spec.Base}, nil
		case slash:
			return CopyPlan{}, fmt.Errorf("%s: no such directory", dest)
		}
		return CopyPlan{filepath.Dir(dest), filepath.Base(dest)}, parentExists(dest)
	}
	switch {
	case exists && !isDir:
		return CopyPlan{}, fmt.Errorf("%s is a file: a directory cannot go over it", dest)
	case spec.Contents && !exists:
		if err := parentExists(dest); err != nil {
			return CopyPlan{}, err
		}
		return CopyPlan{dest, ""}, os.Mkdir(dest, 0o755)
	case spec.Contents:
		return CopyPlan{dest, ""}, nil
	case isDir:
		return CopyPlan{dest, spec.Base}, nil
	}
	return CopyPlan{filepath.Dir(dest), filepath.Base(dest)}, parentExists(dest)
}

func parentExists(path string) error {
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		return fmt.Errorf("%s: no such directory", filepath.Dir(path))
	}
	return nil
}

// SpecOf describes src for a copy: SRC/. is its contents. With follow, a
// link as src is copied as what it points to, under its own name.
func SpecOf(src string, follow bool) (CopySpec, error) {
	contents := strings.HasSuffix(src, "/.") || src == "."
	stat := os.Lstat
	if follow {
		stat = os.Stat
	}
	info, err := stat(filepath.Clean(src))
	if err != nil {
		return CopySpec{}, err
	}
	base := filepath.Base(filepath.Clean(src))
	if contents && !info.IsDir() {
		return CopySpec{}, fmt.Errorf("%s is not a directory", filepath.Dir(src))
	}
	return CopySpec{Base: base, Dir: info.IsDir(), Contents: contents, Follow: follow}, nil
}

// WriteTar writes src as spec says to w.
func WriteTar(w io.Writer, src string, spec CopySpec) error {
	tw := tar.NewWriter(w)
	root := filepath.Clean(src)
	if spec.Follow {
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			return err
		}
		root = resolved
	}
	top := spec.Base
	if spec.Contents {
		top = "."
	}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		name := path.Join(top, filepath.ToSlash(rel))
		if spec.Contents && rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil // a socket or a device: docker cp leaves them too
		}
		hdr.Name = name
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

// ExtractTar unpacks r as plan says. ready, if given, sees the first entry
// before anything is written, and returns the plan: the side that reads a
// copy out learns what it is from the stream. Nothing lands outside the
// plan's directory: not through "..", nor through a link an entry made.
func ExtractTar(r io.Reader, plan CopyPlan, ready func(first *tar.Header) (CopyPlan, error)) error {
	tr := tar.NewReader(r)
	first := true
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if first && ready != nil {
			if plan, err = ready(hdr); err != nil {
				return err
			}
		}
		first = false
		rel, err := landing(hdr.Name, plan.Name)
		if err != nil {
			return err
		}
		if rel == "" {
			continue // the directory a copy of contents goes into
		}
		target, err := secureJoin(plan.Dir, rel)
		if err != nil {
			return err
		}
		mode := hdr.FileInfo().Mode().Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.Mkdir(target, mode|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
		case tar.TypeReg:
			os.Remove(target) // a link in the way is replaced, not followed
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		case tar.TypeSymlink:
			os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		}
	}
}

// landing is where an entry goes, relative to the plan's directory: its top
// name becomes the plan's name, and "." entries go in as they are.
func landing(name, as string) (string, error) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if as == "" { // contents: entries came as ./x, and "." is the directory
		return name, nil
	}
	top, rest, _ := strings.Cut(name, "/")
	if top == "" {
		return "", nil
	}
	if rest == "" {
		return as, nil
	}
	return path.Join(as, rest), nil
}

// secureJoin is dir/rel, where no directory on the way is a link: an entry
// that made a link cannot send a later one elsewhere.
func secureJoin(dir, rel string) (string, error) {
	if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s leaves the destination", rel)
	}
	parts := strings.Split(rel, "/")
	at := dir
	for _, part := range parts[:len(parts)-1] {
		at = filepath.Join(at, part)
		info, err := os.Lstat(at)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("%s goes through %s, which is not a directory", rel, at)
		}
	}
	return filepath.Join(at, parts[len(parts)-1]), nil
}
