package core

import ()

const (
	prefetchWindow = 3 // consecutive sequential reads before prefetching
	prefetchAhead  = 2 // blocks fetched ahead of a sequential reader
)
