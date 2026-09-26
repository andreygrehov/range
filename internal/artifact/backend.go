package artifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"

	"github.com/andreygrehov/range/internal/object"
)

// Backend presents the logical address space of a Range artifact as an
// ordinary byte range source, so the block cache, profiles, NBD server and
// everything else above it are unchanged: they still see a plain disk image.
type Backend struct {
	inner   object.Backend
	uri     string
	header  Header
	entries []Entry
	Object  object.Info

	decoders sync.Pool

	// Compressed traffic is counted separately from the logical bytes the
	// layers above account for, or amplification stops meaning anything.
	CompressedBytes atomic.Int64
	CompressedReqs  atomic.Int64
}

// Open probes a remote object for the artifact magic and, if present,
// wraps it. Only objects without the magic are left alone (nil, nil); failures
// reading or decoding a Range artifact must not turn it into a raw disk image.
func Open(ctx context.Context, inner object.Backend, uri string) (*Backend, error) {
	info, err := inner.Stat(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("range artifact: stat: %w", err)
	}
	if info.Size < int64(len(Magic)) {
		return nil, nil
	}
	head, err := inner.ReadRange(ctx, uri, 0, min(info.Size, headerSize), info.VersionID, info.ETag)
	if err != nil {
		return nil, fmt.Errorf("range artifact: read header: %w", err)
	}
	if !bytes.HasPrefix(head, []byte(Magic)) {
		return nil, nil
	}
	if len(head) < headerSize {
		return nil, errors.New("range artifact: truncated header")
	}
	header, err := ParseHeader(head)
	if err != nil {
		return nil, fmt.Errorf("range artifact: parse header: %w", err)
	}
	if err := header.checkLayout(info.Size); err != nil {
		return nil, err
	}
	packed, err := inner.ReadRange(ctx, uri, header.IndexOffset, header.IndexLength, info.VersionID, info.ETag)
	if err != nil {
		return nil, fmt.Errorf("range artifact: read index: %w", err)
	}
	if int64(len(packed)) != header.IndexLength {
		return nil, fmt.Errorf("range artifact: read index: got %d bytes, want %d", len(packed), header.IndexLength)
	}
	// The decoder is capped at the size the header already committed to, so a
	// small index cannot decompress into gigabytes.
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(uint64(max(header.ChunkCount*entrySize, zstd.MinWindowSize))))
	if err != nil {
		return nil, fmt.Errorf("range artifact: create index decoder: %w", err)
	}
	raw, err := dec.DecodeAll(packed, nil)
	dec.Close()
	if err != nil {
		return nil, fmt.Errorf("range artifact: decode index: %w", err)
	}
	entries, err := ParseIndex(raw)
	if err != nil {
		return nil, err
	}
	if int64(len(entries)) != header.ChunkCount {
		return nil, fmt.Errorf("range artifact: index has %d chunks, header declares %d", len(entries), header.ChunkCount)
	}
	if err := header.checkEntries(entries); err != nil {
		return nil, err
	}
	b := &Backend{inner: inner, uri: uri, header: header, entries: entries, Object: info}
	b.decoders.New = func() any {
		d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
			zstd.WithDecoderMaxMemory(uint64(header.ChunkSize)))
		if err != nil {
			return nil
		}
		return d
	}
	return b, nil
}

// Stat reports the logical size, but keeps the underlying object's identity so
// immutability is still pinned by ETag or version.
func (b *Backend) Stat(ctx context.Context, uri string) (object.Info, error) {
	info := b.Object
	info.Size = b.header.LogicalSize
	return info, nil
}

// ReadRange returns logical bytes [offset, offset+length), fetching the stored
// chunks that cover them - adjacent ones in one request - and checking each
// against its hash.
func (b *Backend) ReadRange(ctx context.Context, uri string, offset, length int64,
	versionID, etag string) ([]byte, error) {

	if offset < 0 || length < 0 || offset+length > b.header.LogicalSize {
		return nil, fmt.Errorf("range artifact: read %d+%d outside logical size %d",
			offset, length, b.header.LogicalSize)
	}
	out := make([]byte, length)
	size := b.header.ChunkSize
	first, last := offset/size, (offset+length-1)/size
	if length == 0 {
		return out, nil
	}
	// Chunks written next to each other are fetched in one request, the same
	// rule the block layer uses for logical reads.
	for i := first; i <= last; {
		if b.entries[i].Zero() {
			i++ // nothing stored, and out is already zero
			continue
		}
		j := i
		for j+1 <= last && !b.entries[j+1].Zero() &&
			b.entries[j+1].Offset == b.entries[j].Offset+int64(b.entries[j].Length) {
			j++
		}
		start := b.entries[i].Offset
		span := b.entries[j].Offset + int64(b.entries[j].Length) - start
		packed, err := b.inner.ReadRange(ctx, uri, start, span, versionID, etag)
		if err != nil {
			return nil, err
		}
		if int64(len(packed)) != span {
			return nil, fmt.Errorf("%w: chunks %d..%d: got %d bytes, want %d",
				errChunkCorrupt, i, j, len(packed), span)
		}
		b.CompressedBytes.Add(span)
		b.CompressedReqs.Add(1)
		for k := i; k <= j; k++ {
			entry := b.entries[k]
			piece := packed[entry.Offset-start : entry.Offset-start+int64(entry.Length)]
			chunk, err := b.decode(piece, k)
			if err != nil {
				return nil, err
			}
			b.copyChunk(out, offset, length, k, chunk)
		}
		i = j + 1
	}
	return out, nil
}

// decode turns one stored chunk back into its logical bytes and checks it
// against the hash recorded in the index. The index is covered by the object's
// own ETag, so a chunk that hashes wrong means the bytes are wrong: a corrupted
// object, a truncated range response, or the wrong object entirely. Serving it
// would put silent corruption inside a filesystem, so it is an error.
func (b *Backend) decode(piece []byte, index int64) ([]byte, error) {
	chunk := piece
	if b.entries[index].Flags&flagRaw == 0 {
		dec, _ := b.decoders.Get().(*zstd.Decoder)
		if dec == nil {
			return nil, errors.New("range artifact: no decoder")
		}
		out, err := dec.DecodeAll(piece, nil)
		b.decoders.Put(dec)
		if err != nil {
			return nil, fmt.Errorf("%w: chunk %d: %v", errChunkCorrupt, index, err)
		}
		chunk = out
	}
	if want := b.header.chunkLength(index); int64(len(chunk)) != want {
		return nil, fmt.Errorf("%w: chunk %d is %d bytes, want %d", errChunkCorrupt, index, len(chunk), want)
	}
	if sum := sha256.Sum256(chunk); sum != b.entries[index].Hash {
		return nil, fmt.Errorf("%w: chunk %d hashes to %s, index says %s",
			errChunkCorrupt, index, hex.EncodeToString(sum[:8]),
			hex.EncodeToString(b.entries[index].Hash[:8]))
	}
	return chunk, nil
}

// copyChunk places the wanted part of one decompressed chunk into the output.
func (b *Backend) copyChunk(out []byte, offset, length, index int64, chunk []byte) {
	size := b.header.ChunkSize
	chunkStart := index * size
	from := int64(0)
	if offset > chunkStart {
		from = offset - chunkStart
	}
	to := int64(len(chunk))
	if end := offset + length - chunkStart; end < to {
		to = end
	}
	if from >= to {
		return
	}
	copy(out[chunkStart+from-offset:], chunk[from:to])
}
