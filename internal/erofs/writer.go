package erofs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"syscall"
)

// WriteNodes lays out and writes an image from a prepared tree: nodes in
// the order their inodes should appear, root first, directories carrying their
// entries and parent.
func WriteNodes(rootNode *Node, nodes []*Node, output string, align int64) (Stats, error) {
	var st Stats
	if align > 0 && align%BlockSize != 0 {
		return st, fmt.Errorf("erofs: alignment %d is not a multiple of %d", align, BlockSize)
	}

	// Inodes are 64 bytes and start at block 1, so node i sits at nid
	// 128 + 2i. The root comes first, which keeps its nid inside the 16-bit
	// root_nid field of the superblock.
	for i, node := range nodes {
		node.nid = uint64(BlockSize>>SlotBits) + uint64(i)*(InodeSize>>SlotBits)
	}
	for _, node := range nodes {
		if node.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			node.dirData, node.Size = dirBlocks(node)
		}
	}

	blocks := func(n int64) int64 { return (n + BlockSize - 1) / BlockSize }
	cursor := blocks(BlockSize + int64(len(nodes))*InodeSize)
	place := func(node *Node) {
		node.blkaddr = uint32(cursor)
		cursor += blocks(node.Size)
	}
	for _, node := range nodes {
		if node.Mode&syscall.S_IFMT == syscall.S_IFDIR {
			place(node)
		}
	}
	for _, node := range nodes {
		if node.Mode&syscall.S_IFMT == syscall.S_IFLNK {
			place(node)
		}
	}
	alignBlocks := align / BlockSize
	roundUp := func() {
		if alignBlocks > 0 && cursor%alignBlocks != 0 {
			cursor += alignBlocks - cursor%alignBlocks
		}
	}
	for _, node := range nodes {
		if node.Mode&syscall.S_IFMT != syscall.S_IFREG || node.Size == 0 {
			continue
		}
		node.aligned = align > 0 && node.Size >= align
		if node.aligned {
			roundUp()
			st.AlignedFiles++
		}
		place(node)
		if node.aligned {
			roundUp()
		}
	}
	if cursor > math.MaxUint32 {
		return st, errors.New("erofs: image would exceed 16 TiB")
	}

	file, err := os.Create(output)
	if err != nil {
		return st, err
	}
	defer file.Close()
	size := cursor * BlockSize
	if err := file.Truncate(size); err != nil {
		return st, err
	}

	super := make([]byte, 128)
	binary.LittleEndian.PutUint32(super[0:], Magic)
	super[12] = BlockBits
	binary.LittleEndian.PutUint16(super[14:], uint16(rootNode.nid))
	binary.LittleEndian.PutUint64(super[16:], uint64(len(nodes)))
	binary.LittleEndian.PutUint32(super[36:], uint32(cursor))
	copy(super[64:80], "range")
	if _, err := file.WriteAt(super, SuperOffset); err != nil {
		return st, err
	}

	table := make([]byte, len(nodes)*InodeSize)
	for i, node := range nodes {
		inode := table[i*InodeSize:]
		binary.LittleEndian.PutUint16(inode[0:], FormatExtended)
		binary.LittleEndian.PutUint16(inode[4:], uint16(node.Mode))
		binary.LittleEndian.PutUint64(inode[8:], uint64(node.Size))
		switch node.Mode & syscall.S_IFMT {
		case syscall.S_IFCHR, syscall.S_IFBLK:
			binary.LittleEndian.PutUint32(inode[16:], node.Rdev)
		case syscall.S_IFREG, syscall.S_IFDIR, syscall.S_IFLNK:
			binary.LittleEndian.PutUint32(inode[16:], node.blkaddr)
		}
		binary.LittleEndian.PutUint32(inode[20:], uint32(i+1))
		binary.LittleEndian.PutUint32(inode[24:], node.Uid)
		binary.LittleEndian.PutUint32(inode[28:], node.Gid)
		binary.LittleEndian.PutUint64(inode[32:], uint64(node.Mtime.Unix()))
		binary.LittleEndian.PutUint32(inode[40:], uint32(node.Mtime.Nanosecond()))
		binary.LittleEndian.PutUint32(inode[44:], node.Nlink)
	}
	if _, err := file.WriteAt(table, BlockSize); err != nil {
		return st, err
	}

	buf := make([]byte, 1<<20)
	for _, node := range nodes {
		at := int64(node.blkaddr) * BlockSize
		switch node.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR:
			if _, err := file.WriteAt(node.dirData, at); err != nil {
				return st, err
			}
		case syscall.S_IFLNK:
			if _, err := file.WriteAt([]byte(node.Target), at); err != nil {
				return st, err
			}
		case syscall.S_IFREG:
			if node.Size == 0 || node.External {
				continue
			}
			in, err := os.Open(node.Source)
			if err != nil {
				return st, err
			}
			written, err := io.CopyBuffer(io.NewOffsetWriter(file, at), io.LimitReader(in, node.Size), buf)
			in.Close()
			if err != nil {
				return st, err
			}
			if written != node.Size {
				return st, fmt.Errorf("erofs: %s changed size while being read", node.Source)
			}
		}
	}
	if err := file.Sync(); err != nil {
		return st, err
	}
	st.Inodes, st.Blocks, st.Size = len(nodes), cursor, size
	return st, nil
}

// dirBlocks lays out one directory: entries sorted bytewise, "." and ".."
// included, packed into blocks with each block's dirents first and its names
// after them. The kernel binary-searches names within and across blocks, so
// the order is load-bearing. The returned size is exact: the kernel treats the
// end of the last name in the last block as i_size.
func dirBlocks(dir *Node) ([]byte, int64) {
	entries := append([]Dirent{{Name: ".", Node: dir}, {Name: "..", Node: dir.Parent}}, dir.Entries...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })

	var groups [][]Dirent
	var current []Dirent
	used := 0
	for _, entry := range entries {
		need := DirentSize + len(entry.Name)
		if used+need > BlockSize {
			groups = append(groups, current)
			current, used = nil, 0
		}
		current = append(current, entry)
		used += need
	}
	groups = append(groups, current)

	data := make([]byte, len(groups)*BlockSize)
	var last int
	for g, group := range groups {
		block := data[g*BlockSize:]
		nameoff := len(group) * DirentSize
		for i, entry := range group {
			d := block[i*DirentSize:]
			binary.LittleEndian.PutUint64(d[0:], entry.Node.nid)
			binary.LittleEndian.PutUint16(d[8:], uint16(nameoff))
			d[10] = fileType(entry.Node.Mode)
			copy(block[nameoff:], entry.Name)
			nameoff += len(entry.Name)
		}
		last = nameoff
	}
	return data, int64(len(groups)-1)*BlockSize + int64(last)
}
