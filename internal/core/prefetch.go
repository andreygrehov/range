package core

const (
	prefetchWindow   = 3  // consecutive sequential blocks before prefetching
	prefetchAhead    = 2  // blocks fetched ahead once a reader turns sequential
	prefetchMaxAhead = 32 // the window doubles per block the reader keeps walking, up to this
)
