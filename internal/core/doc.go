// Package core is Range's block layer. It knows nothing about what its bytes
// mean; it understands only
//
//	Size() -> N
//	ReadAt(offset, length) -> bytes
//
// and everything above that - EROFS, NBD, VMs - is somebody else's
// abstraction.
//
// A Reader turns reads into fixed-size block fetches from an object.Backend,
// served from the memory and disk caches when it can, with concurrent requests
// for the same block collapsed into one, adjacent misses coalesced, sequential
// runs prefetched, and a learned working-set profile replayed ahead of demand
// without ever delaying it. It keeps the statistics range reports.
package core
