# Range Artifact v1

A Range artifact is a single immutable object. It presents a large logical
address space, but it stores only the chunks that hold data, in compressed form.
It is designed so that another runtime can read it. Everything that a reader
needs is below.

```
┌────────────────────────────┐ 0
│ header            64 bytes │
├────────────────────────────┤ 64
│ compressed chunks          │  written in logical order
├────────────────────────────┤ header.indexOffset
│ chunk index (zstd)         │
└────────────────────────────┘ end of object
```

To open an artifact, a reader makes two small range requests: the header, then
the index. After that, a logical read is one range request for the chunks that
it covers.

## Header

The header is 64 bytes, little-endian, at offset 0.

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 8 | magic, ASCII `RANGEAR1` |
| 8 | 4 | format version, currently `1` |
| 12 | 4 | reserved, zero |
| 16 | 8 | `chunkSize`, logical bytes per chunk |
| 24 | 8 | `logicalSize`, size of the address space the artifact presents |
| 32 | 8 | `indexOffset`, byte offset of the index in this object |
| 40 | 8 | `indexLength`, compressed length of the index |
| 48 | 8 | `chunkCount`, number of index entries |
| 56 | 8 | `storedChunks`, entries that occupy bytes |

A reader must reject an artifact if any of these conditions is true:

- The magic does not match.
- The reader does not implement the format version.
- A field fails the checks under [Validation](#validation).

## Index

The index is `indexLength` bytes at `indexOffset`, compressed with zstd. The
uncompressed index is `chunkCount` fixed 48-byte entries, little-endian, in
logical order. Entry *i* describes logical bytes
`[i*chunkSize, (i+1)*chunkSize)`, clipped to `logicalSize`.

| Offset | Size | Field |
| ---: | ---: | --- |
| 0 | 8 | `offset`, byte offset of the stored chunk in this object |
| 8 | 4 | `length`, stored length in bytes |
| 12 | 4 | `flags` |
| 16 | 32 | `hash`, SHA-256 of the **uncompressed** chunk |

Flags:

| Bit | Meaning |
| ---: | --- |
| 0 | the chunk is stored uncompressed, because compressing it made it larger |

## Zero chunks

`length == 0` means that every byte of the chunk is zero and that the chunk
occupies no bytes in the object. This document calls such a chunk a *zero
chunk*. For a zero chunk, a reader returns zeroes and sends no request.

Zero chunks keep a sparse filesystem sparse in object storage. For example, a
20 GiB ext4 image that holds 2 GiB of data is a 619 MB artifact, not a 20 GiB
one.

## Content identity

`hash` is the SHA-256 of the chunk's uncompressed bytes. Identity is therefore
a property of the content. It does not depend on how the writer compressed the
chunk or where the writer stored it.

Two entries may point at the same `offset` and `length`. A writer that sees a
chunk it already stored reuses that chunk. Identity is stable across artifact
versions. That stability is the foundation for a future cross-version profile
or a shared chunk store. Neither exists yet.

## Reading

To serve `ReadAt(offset, length)`:

1. `first = offset / chunkSize`, `last = (offset + length - 1) / chunkSize`.
2. For each entry in `[first, last]`, a zero chunk contributes zeroes. A reader
   may fetch a run of entries that are adjacent in the object in one range
   request.
3. Decompress each stored chunk, unless flag 0 is set.
4. Copy the requested part of each chunk.

The writer compresses each chunk independently. A reader can therefore
decompress any chunk without reading any other chunk.

## Integrity

`hash` is the SHA-256 of a chunk's uncompressed bytes. The reference reader
checks the hash of every chunk that it decompresses. On a mismatch, it fails the
read. As a result, a corrupted object, a truncated response or the wrong object
never becomes filesystem data.

The object's identity (ETag or object version) covers the index itself. The
reader pins that identity at open.

## Validation

An artifact can come from any URL. A reader therefore treats the header and the
index as untrusted. It checks them before it allocates anything that they ask
for. The reference reader rejects an artifact unless all of these conditions
are true:

- `chunkSize` is between 4 KiB and 64 MiB, inclusive
- `chunkCount == ceil(logicalSize / chunkSize)`, and `chunkCount` is at most
  2^22 (4 TiB at 1 MiB chunks)
- `indexOffset >= 64`, `indexLength > 0`, and `indexOffset + indexLength` lies
  within the object
- `indexLength` is no larger than a compressed index of `chunkCount` entries
  could be
- the index decompresses to exactly `chunkCount * 48` bytes (the reference
  reader caps the decompressor at that size, so a small index cannot expand
  into gigabytes)
- every stored entry has `0 < length <= chunkSize` and lies between the header
  and the index
- every entry has only known flag bits set

On each read, the reference reader also rejects a range response that is
shorter than the requested length. It also rejects a chunk whose decompressed
length is not `min(chunkSize, logicalSize - i * chunkSize)`. The reference
reader caps chunk decompression at `chunkSize`.

## Compatibility rules

- The header is fixed at 64 bytes for all v1 artifacts.
- Index entries are fixed at 48 bytes for all v1 artifacts.
- A reader must reject an entry with flag bits it does not understand. v1
  defines only bit 0.
- New fields go in a new format version, not in the reserved bytes.
- Artifacts are immutable. A change produces a new object with a new identity.
  A reader pins that identity by ETag or object version at open, exactly as it
  pins a raw image.

## What v1 deliberately leaves out

v1 does not include these features:

- content-defined chunking
- a shared chunk store across artifacts (a writer deduplicates chunks within one
  artifact only)
- encryption
