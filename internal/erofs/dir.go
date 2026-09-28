package erofs

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// WriteDir builds an uncompressed EROFS image of a directory at output,
// taking ownership, modes and times from the files themselves. align is the
// artifact chunk size; zero disables chunk alignment.
func WriteDir(root, output string, align int64) (Stats, error) {
	rootNode, nodes, err := treeFromDir(root)
	if err != nil {
		return Stats{}, err
	}
	return WriteNodes(rootNode, nodes, output, align)
}

// treeFromDir walks a directory into nodes, root first, in name order.
func treeFromDir(root string) (*Node, []*Node, error) {
	var nodes []*Node
	hardlinks := map[[2]uint64]*Node{}

	var walk func(path string, info os.FileInfo, parent *Node) (*Node, error)
	walk = func(path string, info os.FileInfo, parent *Node) (*Node, error) {
		sys, _ := info.Sys().(*syscall.Stat_t)
		node := &Node{Source: path, Parent: parent, Mtime: info.ModTime(), Nlink: 1}
		node.Mode = uint32(info.Mode().Perm())
		if sys != nil {
			node.Mode = uint32(sys.Mode)
			node.Uid, node.Gid = sys.Uid, sys.Gid
		}
		switch {
		case info.Mode().IsRegular():
			if sys != nil && uint64(sys.Nlink) > 1 {
				key := [2]uint64{uint64(sys.Dev), uint64(sys.Ino)}
				if first, ok := hardlinks[key]; ok {
					first.Nlink++
					return first, nil
				}
				hardlinks[key] = node
			}
			node.Size = info.Size()
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return nil, err
			}
			if len(target) >= BlockSize {
				return nil, fmt.Errorf("erofs: symlink %s is too long", path)
			}
			node.Target = target
			node.Size = int64(len(target))
		case info.IsDir():
			node.Nlink = 2
		case info.Mode()&(os.ModeDevice|os.ModeCharDevice) != 0:
			if sys != nil {
				node.Rdev = encodeDev(uint64(sys.Rdev))
			}
		case info.Mode()&(os.ModeNamedPipe|os.ModeSocket) != 0:
		default:
			return nil, nil // nothing else has an EROFS representation
		}
		nodes = append(nodes, node)
		if !info.IsDir() {
			return node, nil
		}
		children, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		for _, child := range children {
			name := child.Name()
			if len(name) > NameMax {
				return nil, fmt.Errorf("erofs: name longer than %d bytes: %s", NameMax, name)
			}
			childPath := filepath.Join(path, name)
			childInfo, err := os.Lstat(childPath)
			if err != nil {
				return nil, err
			}
			childNode, err := walk(childPath, childInfo, node)
			if err != nil {
				return nil, err
			}
			if childNode == nil {
				continue
			}
			if childInfo.IsDir() {
				node.Nlink++
			}
			node.Entries = append(node.Entries, Dirent{Name: name, Node: childNode})
		}
		return node, nil
	}
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, nil, err
	}
	if !rootInfo.IsDir() {
		return nil, nil, fmt.Errorf("erofs: %s is not a directory", root)
	}
	rootNode, err := walk(root, rootInfo, nil)
	if err != nil {
		return nil, nil, err
	}
	rootNode.Parent = rootNode
	return rootNode, nodes, nil
}
