package object

import (
	"testing"
)

func TestIdentitySeparatesVersions(t *testing.T) {
	base := Identity{URI: "s3://b/k", Size: 100, ETag: "aaa", BlockSize: 1 << 20}
	changedETag := base
	changedETag.ETag = "bbb"
	changedSize := base
	changedSize.Size = 101
	changedBlock := base
	changedBlock.BlockSize = 4 << 20

	if base.Key() == changedETag.Key() {
		t.Fatal("a different ETag must produce a different cache key")
	}
	if base.Key() == changedSize.Key() {
		t.Fatal("a different size must produce a different cache key")
	}
	if base.Key() == changedBlock.Key() {
		t.Fatal("a different block size must produce a different cache key")
	}
	if base.Key() != base.Key() {
		t.Fatal("cache keys must be stable")
	}
}
