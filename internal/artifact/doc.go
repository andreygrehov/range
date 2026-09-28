// Package artifact implements Range Artifact v1: one object that presents a
// large logical address space while storing only the chunks that hold data,
// compressed:
//
//	┌────────────────────────────┐ 0
//	│ header (64 B, fixed)       │
//	├────────────────────────────┤
//	│ zstd chunk, zstd chunk, …  │
//	├────────────────────────────┤ header.IndexOffset
//	│ chunk index (zstd)         │
//	└────────────────────────────┘
//
// Opening costs two small reads: the header, then the index. After that every
// logical read is a range request for the chunks it covers. A chunk of all
// zeroes is not stored at all, which keeps a sparse filesystem sparse in object
// storage, and identical chunks are stored once. Every chunk is checked against
// the SHA-256 recorded in the index before it is returned.
//
// Backend presents an artifact's logical bytes as an ordinary object.Backend,
// so everything above it sees a plain disk image. Pack writes one. The format
// is specified in docs/ARTIFACT_FORMAT.md so another runtime can read it.
package artifact
