package artifact

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// errChunkCorrupt is returned when stored bytes do not match the identity the
// index recorded for them.
var errChunkCorrupt = errors.New("range artifact: chunk does not match its hash")

const (
	Magic      = "RANGEAR1"
	version    = 1
	headerSize = 64
	entrySize  = 48 // offset(8) + length(4) + flags(4) + hash(32)

	// flagRaw marks a chunk stored uncompressed because compressing it
	// made it bigger.
	flagRaw = 1 << 0

	// Limits a reader enforces before it allocates anything a header asks for.
	// An artifact is untrusted input: it can come from any URL.
	minChunk  = 4 << 10
	maxChunk  = 64 << 20
	maxChunks = 1 << 22 // 4 TiB at 1 MiB chunks; a 192 MiB index
)

// Header is the fixed 64-byte header at the start of every artifact.
type Header struct {
	ChunkSize    int64
	LogicalSize  int64
	IndexOffset  int64
	IndexLength  int64 // compressed
	ChunkCount   int64
	StoredChunks int64
}

func (h Header) marshal() []byte {
	buf := make([]byte, headerSize)
	copy(buf, Magic)
	binary.LittleEndian.PutUint32(buf[8:], version)
	binary.LittleEndian.PutUint64(buf[16:], uint64(h.ChunkSize))
	binary.LittleEndian.PutUint64(buf[24:], uint64(h.LogicalSize))
	binary.LittleEndian.PutUint64(buf[32:], uint64(h.IndexOffset))
	binary.LittleEndian.PutUint64(buf[40:], uint64(h.IndexLength))
	binary.LittleEndian.PutUint64(buf[48:], uint64(h.ChunkCount))
	binary.LittleEndian.PutUint64(buf[56:], uint64(h.StoredChunks))
	return buf
}

// ParseHeader decodes and validates a header. It rejects anything a reader
// should not allocate for: see docs/ARTIFACT_FORMAT.md, Validation.
func ParseHeader(buf []byte) (Header, error) {
	var h Header
	if len(buf) < headerSize || string(buf[:8]) != Magic {
		return h, errors.New("not a range artifact")
	}
	if v := binary.LittleEndian.Uint32(buf[8:]); v != version {
		return h, fmt.Errorf("range artifact version %d is not supported", v)
	}
	h.ChunkSize = int64(binary.LittleEndian.Uint64(buf[16:]))
	h.LogicalSize = int64(binary.LittleEndian.Uint64(buf[24:]))
	h.IndexOffset = int64(binary.LittleEndian.Uint64(buf[32:]))
	h.IndexLength = int64(binary.LittleEndian.Uint64(buf[40:]))
	h.ChunkCount = int64(binary.LittleEndian.Uint64(buf[48:]))
	h.StoredChunks = int64(binary.LittleEndian.Uint64(buf[56:]))
	if h.ChunkSize < minChunk || h.ChunkSize > maxChunk {
		return h, fmt.Errorf("range artifact: chunk size %d is outside %d..%d",
			h.ChunkSize, minChunk, maxChunk)
	}
	if h.LogicalSize < 0 || h.ChunkCount < 0 || h.ChunkCount > maxChunks ||
		h.LogicalSize > h.ChunkCount*h.ChunkSize ||
		h.ChunkCount != (h.LogicalSize+h.ChunkSize-1)/h.ChunkSize {
		return h, fmt.Errorf("range artifact: %d chunks of %d bytes cannot hold %d bytes",
			h.ChunkCount, h.ChunkSize, h.LogicalSize)
	}
	return h, nil
}

// checkLayout confirms the header describes an object of this size: chunk data
// after the header, then an index that ends inside the object and is no larger
// than a compressed index of ChunkCount entries could be.
func (h Header) checkLayout(objectSize int64) error {
	raw := h.ChunkCount * entrySize
	if h.IndexOffset < headerSize || h.IndexLength <= 0 ||
		h.IndexLength > raw+raw/64+1024 ||
		h.IndexOffset > objectSize || h.IndexLength > objectSize-h.IndexOffset {
		return fmt.Errorf("range artifact: index at %d+%d does not fit a %d byte object",
			h.IndexOffset, h.IndexLength, objectSize)
	}
	return nil
}

// checkEntries confirms every stored chunk lies between the header and the
// index, is no bigger than a chunk, and carries only flags this reader knows.
func (h Header) checkEntries(entries []Entry) error {
	for i, e := range entries {
		if e.Flags&^flagRaw != 0 {
			return fmt.Errorf("range artifact: chunk %d has unknown flags %#x", i, e.Flags)
		}
		if e.Zero() {
			continue
		}
		if e.Length < 0 || int64(e.Length) > h.ChunkSize || e.Offset < headerSize ||
			e.Offset > h.IndexOffset-int64(e.Length) {
			return fmt.Errorf("range artifact: chunk %d at %d+%d lies outside the chunk data",
				i, e.Offset, e.Length)
		}
	}
	return nil
}

// chunkLength is the logical length of chunk i; only the last one is short.
func (h Header) chunkLength(i int64) int64 {
	return min(h.ChunkSize, h.LogicalSize-i*h.ChunkSize)
}

// Entry locates one logical chunk. A zero Length means the chunk is all
// zeroes and occupies nothing.
type Entry struct {
	Offset int64
	Length int32
	Flags  uint32
	Hash   [32]byte
}

// Zero reports whether the chunk is all zeroes and stored nowhere.
func (e Entry) Zero() bool { return e.Length == 0 }

// MarshalIndex encodes index entries in their fixed 48-byte form.
func MarshalIndex(entries []Entry) []byte {
	buf := make([]byte, len(entries)*entrySize)
	for i, e := range entries {
		at := buf[i*entrySize:]
		binary.LittleEndian.PutUint64(at, uint64(e.Offset))
		binary.LittleEndian.PutUint32(at[8:], uint32(e.Length))
		binary.LittleEndian.PutUint32(at[12:], e.Flags)
		copy(at[16:], e.Hash[:])
	}
	return buf
}

// ParseIndex decodes an uncompressed index.
func ParseIndex(buf []byte) ([]Entry, error) {
	if len(buf)%entrySize != 0 {
		return nil, errors.New("range artifact index is truncated")
	}
	entries := make([]Entry, len(buf)/entrySize)
	for i := range entries {
		at := buf[i*entrySize:]
		entries[i].Offset = int64(binary.LittleEndian.Uint64(at))
		entries[i].Length = int32(binary.LittleEndian.Uint32(at[8:]))
		entries[i].Flags = binary.LittleEndian.Uint32(at[12:])
		copy(entries[i].Hash[:], at[16:48])
	}
	return entries, nil
}
