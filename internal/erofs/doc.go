// Package erofs writes EROFS images. The base of an environment is never
// written to, so a filesystem built for writing - ext4, with a journal and an
// allocator - is the wrong tool, and it costs a dependency on e2fsprogs. EROFS
// is the kernel's read-only filesystem, in mainline since 5.4, and its
// uncompressed form is simple enough to write directly: compression already
// happens one layer down, in the artifact's chunks.
//
// The writer emits:
//
//	block 0          superblock at byte 1024
//	block 1..        inode table, 64-byte extended inodes, root first
//	then             every directory block, then symlink targets
//	then             file data in tree order
//
// Metadata sits together at the front, so a cold start touches a handful of
// chunks to resolve paths. Files at least one chunk long start on a chunk
// boundary and are padded to the next one, so an identical file lands in
// identical chunks in any artifact that contains it - which is what makes a
// toolchain shared by a thousand environments dedupable at all. Smaller files
// are not aligned: the padding would be most of their chunk, and the cache
// decompresses and stores logical bytes, zeros included. Measured on a Go
// build, aligning from a quarter chunk grew the working set 38% and made a
// profiled start 30% slower for 11% fewer bytes over the network. The layout is
// a pure function of the source tree: same tree, same bytes.
//
// Not written: extended attributes (file capabilities are lost, as they are in
// the ext4 path), inline tail packing, and compression.
package erofs
