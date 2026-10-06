package virtual

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// An image built once can be saved and loaded again, so it is not laid out
// anew every time it is opened. What is saved is its size, the parts held in
// memory, and each extent with its tag; the readers are the caller's to give
// again. A SHA-256 of everything before it closes the file, so a file cut
// short or changed is refused, not served.

var saveMagic = [8]byte{'R', 'V', 'I', 'M', 'G', 0, 0, 1}

// Save writes the image's layout to w.
func (img *Image) Save(w io.Writer) error {
	sum := sha256.New()
	out := bufio.NewWriter(io.MultiWriter(w, sum))
	put := func(v any) { binary.Write(out, binary.LittleEndian, v) }
	put(saveMagic)
	put(img.Size)
	put(uint64(len(img.local)))
	for _, r := range img.local {
		put(r.start)
		put(uint64(len(r.data)))
		out.Write(r.data)
	}
	put(uint64(len(img.Extents)))
	for _, e := range img.Extents {
		put([4]int64{e.Start, e.Size, e.Tag[0], e.Tag[1]})
	}
	if err := out.Flush(); err != nil {
		return err
	}
	_, err := w.Write(sum.Sum(nil))
	return err
}

// errBadSave is a saved image that is not one, or not whole.
var errBadSave = errors.New("virtual: not a whole saved image")

// Load reads an image Save wrote. read returns the reader for a tag.
func Load(data []byte, read func(Tag) ReadFunc) (*Image, error) {
	if len(data) < sha256.Size {
		return nil, errBadSave
	}
	body, sum := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	if got := sha256.Sum256(body); !bytes.Equal(got[:], sum) {
		return nil, errBadSave
	}
	in := bytes.NewReader(body)
	var err error
	get := func(v any) {
		if err == nil {
			err = binary.Read(in, binary.LittleEndian, v)
		}
	}
	var magic [8]byte
	get(&magic)
	if err != nil || magic != saveMagic {
		return nil, fmt.Errorf("virtual: saved image of another format")
	}
	img := &Image{Concurrency: concurrency}
	var regions, extents uint64
	get(&img.Size)
	get(&regions)
	for i := uint64(0); i < regions && err == nil; i++ {
		var start int64
		var n uint64
		get(&start)
		get(&n)
		if err == nil && n > uint64(in.Len()) {
			return nil, errBadSave
		}
		data := make([]byte, n)
		if err == nil {
			_, err = io.ReadFull(in, data)
		}
		img.local = append(img.local, region{start: start, data: data})
	}
	get(&extents)
	if err == nil && extents > uint64(in.Len())/32 {
		return nil, errBadSave
	}
	img.Extents = make([]Extent, 0, extents)
	for i := uint64(0); i < extents && err == nil; i++ {
		var e [4]int64
		get(&e)
		tag := Tag{e[2], e[3]}
		img.Extents = append(img.Extents, Extent{Start: e[0], Size: e[1], Tag: tag, Read: read(tag)})
	}
	if err != nil || in.Len() != 0 {
		return nil, errBadSave
	}
	return img, nil
}
