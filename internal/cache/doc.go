// Package cache holds fixed-size blocks: Memory is a small LRU in the process,
// Disk a size-bounded LRU on local disk that survives restarts. Blocks are
// keyed by index; which artifact they belong to is decided by the directory a
// Disk is opened in.
package cache
