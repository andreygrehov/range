package erofs

import (
	"syscall"
	"time"
)

const (
	Magic       = 0xE0F5E1E2
	BlockSize   = 4096
	BlockBits   = 12
	SuperOffset = 1024
	InodeSize   = 64 // extended inode, no xattrs
	SlotBits    = 5
	DirentSize  = 12
	NameMax     = 255

	FormatExtended = 1 // i_format bit 0; data layout bits 1-3 stay FLAT_PLAIN
)

// EROFS directory entry file types.
const (
	FTUnknown = iota
	FTRegular
	FTDir
	FTChr
	FTBlk
	FTFifo
	FTSock
	FTSymlink
)

// Node is one inode to be written: a file, directory, symlink or device.
type Node struct {
	Source   string
	Mode     uint32 // full st_mode
	Uid, Gid uint32
	Mtime    time.Time
	Size     int64
	Rdev     uint32 // new_encode_dev form
	Target   string // symlink target
	Entries  []Dirent
	// External marks a regular file whose data lives somewhere else. The writer
	// gives it blocks but leaves them as a hole; whoever serves the image fills
	// them in, from BlockAddr onward.
	External bool
	Parent   *Node
	nid      uint64
	Nlink    uint32
	blkaddr  uint32
	aligned  bool
	dirData  []byte

	// Used only while an image is assembled from OCI layers: children by name,
	// and the layer each came from, which an opaque whiteout needs.
	Children   map[string]*Node
	ChildLayer map[string]int
}

// Dirent names a Node inside its parent directory.
type Dirent struct {
	Name string
	Node *Node
}

// Stats describes the image a writer produced.
type Stats struct {
	Inodes       int
	Blocks       int64
	Size         int64
	AlignedFiles int
}

func fileType(mode uint32) uint8 {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		return FTRegular
	case syscall.S_IFDIR:
		return FTDir
	case syscall.S_IFCHR:
		return FTChr
	case syscall.S_IFBLK:
		return FTBlk
	case syscall.S_IFIFO:
		return FTFifo
	case syscall.S_IFSOCK:
		return FTSock
	case syscall.S_IFLNK:
		return FTSymlink
	}
	return FTUnknown
}

// encodeDev converts a Linux stat rdev to the kernel's new_encode_dev
// form, which is what EROFS stores.
func encodeDev(rdev uint64) uint32 {
	major := uint32((rdev>>8)&0xfff) | uint32((rdev>>32)&^0xfff)
	minor := uint32(rdev&0xff) | uint32((rdev>>12)&^0xff)
	return (minor & 0xff) | (major << 8) | ((minor &^ 0xff) << 12)
}

// BlockAddr is the first block of a node's data, valid once WriteNodes has laid
// the image out.
func (n *Node) BlockAddr() uint32 { return n.blkaddr }
