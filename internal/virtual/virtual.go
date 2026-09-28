// Package virtual serves an EROFS image that was never written out whole.
//
// Build lays an image out with the erofs writer, keeps what the writer
// actually wrote - the superblock, inodes, directories, and any file whose
// body Range made itself - in memory, and records, for every file marked
// External, where its bytes sit in the image and how to read them. ReadRange
// then assembles any range of the image from memory, from those readers, and
// from the zeros between files.
//
// The image is what a Hugging Face repository or a container image becomes:
// kilobytes of metadata here, and the data left where it already lives.
package virtual

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"syscall"

	"github.com/andreygrehov/range/internal/erofs"
)

// ReadFunc fills dst with a file's bytes starting at off within the file.
type ReadFunc func(ctx context.Context, dst []byte, off int64) error

// Extent is where one external file's bytes sit in the image.
type Extent struct {
	Start, Size int64
	Read        ReadFunc
}

// region is a part of the image held in memory.
type region struct {
	start int64
	data  []byte
}

// Image is a laid-out EROFS image whose file data is read on demand.
type Image struct {
	Size    int64
	local   []region
	Extents []Extent // sorted by Start

	// Concurrency bounds the reads one ReadRange issues at once when it
	// covers several files.
	Concurrency int
}

// Build lays out nodes as erofs.WriteNodes does. For every node marked
// External, locate returns how to read its bytes.
func Build(root *erofs.Node, nodes []*erofs.Node, align int64, locate func(*erofs.Node) ReadFunc) (*Image, error) {
	scratch, err := os.CreateTemp("", "range-virtual-*.erofs")
	if err != nil {
		return nil, err
	}
	scratch.Close()
	defer os.Remove(scratch.Name())
	st, err := erofs.WriteNodes(root, nodes, scratch.Name(), align)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(scratch.Name())
	if err != nil {
		return nil, err
	}
	defer f.Close()

	img := &Image{Size: st.Size, Concurrency: 8}
	// Metadata is the superblock, the inode table, and the directory and
	// symlink blocks, which the writer lays out before any file body. It ends
	// with the last of them, not at the first file, which alignment can push
	// a long way further.
	metaEnd := int64(erofs.BlockSize) + int64(st.Inodes)*erofs.InodeSize
	seen := map[*erofs.Node]bool{}
	for _, node := range nodes {
		if seen[node] {
			continue
		}
		seen[node] = true
		switch node.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR, syscall.S_IFLNK:
			metaEnd = max(metaEnd, int64(node.BlockAddr())*erofs.BlockSize+node.Size)
			continue
		case syscall.S_IFREG:
		default:
			continue
		}
		if node.Size == 0 {
			continue
		}
		start := int64(node.BlockAddr()) * erofs.BlockSize
		if node.External {
			read := locate(node)
			if read == nil {
				return nil, fmt.Errorf("virtual: no reader for an external file of %d bytes", node.Size)
			}
			img.Extents = append(img.Extents, Extent{Start: start, Size: node.Size, Read: read})
			continue
		}
		data := make([]byte, node.Size)
		if _, err := f.ReadAt(data, start); err != nil {
			return nil, err
		}
		img.local = append(img.local, region{start: start, data: data})
	}
	meta := make([]byte, metaEnd)
	if _, err := io.ReadFull(f, meta); err != nil {
		return nil, err
	}
	img.local = append(img.local, region{start: 0, data: meta})
	sort.Slice(img.local, func(i, j int) bool { return img.local[i].start < img.local[j].start })
	sort.Slice(img.Extents, func(i, j int) bool { return img.Extents[i].Start < img.Extents[j].Start })
	return img, nil
}

// MetadataSize is how much of the image Range holds in memory.
func (img *Image) MetadataSize() int64 {
	var n int64
	for _, r := range img.local {
		n += int64(len(r.data))
	}
	return n
}

// ReadRange assembles length bytes of the image from offset.
func (img *Image) ReadRange(ctx context.Context, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 || offset+length > img.Size {
		return nil, fmt.Errorf("virtual: range %d+%d is outside the %d-byte image", offset, length, img.Size)
	}
	buf := make([]byte, length)
	end := offset + length
	for _, r := range img.local {
		if s, e := max(offset, r.start), min(end, r.start+int64(len(r.data))); s < e {
			copy(buf[s-offset:e-offset], r.data[s-r.start:e-r.start])
		}
	}

	first := sort.Search(len(img.Extents), func(i int) bool {
		return img.Extents[i].Start+img.Extents[i].Size > offset
	})
	var wg sync.WaitGroup
	slots := make(chan struct{}, max(img.Concurrency, 1))
	var once sync.Once
	var firstErr error
	for _, ext := range img.Extents[first:] {
		if ext.Start >= end {
			break
		}
		lo, hi := max(offset, ext.Start), min(end, ext.Start+ext.Size)
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			if err := ext.Read(ctx, buf[lo-offset:hi-offset], lo-ext.Start); err != nil {
				once.Do(func() { firstErr = err })
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return buf, nil
}
