package profile

import (
	"time"

	"github.com/andreygrehov/range/internal/object"
)

func testIdentity() object.Identity {
	return object.Identity{URI: "s3://bucket/dev.img", Size: 100 << 20, ETag: "etag-1", BlockSize: 1 << 20}
}

// observe builds one session's observations: block -> first use offset.
func observe(pairs map[int64]int) map[int64]time.Duration {
	out := make(map[int64]time.Duration, len(pairs))
	for block, ms := range pairs {
		out[block] = time.Duration(ms) * time.Millisecond
	}
	return out
}
