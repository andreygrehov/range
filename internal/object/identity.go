package object

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Identity pins an artifact to one immutable version. Cached blocks from two
// different versions must never mix, so the cache key includes the version.
type Identity struct {
	URI          string    `json:"uri"`
	Size         int64     `json:"size"`
	ETag         string    `json:"etag"`
	VersionID    string    `json:"version_id"`
	LastModified time.Time `json:"last_modified"`
	BlockSize    int64     `json:"block_size"`
}

// Key is a stable hash of everything that makes two reads the same artifact.
// Caches and profiles are stored under it.
func (i Identity) Key() string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		i.URI, strconv.FormatInt(i.Size, 10), i.ETag, i.VersionID, strconv.FormatInt(i.BlockSize, 10),
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// SameArtifact reports whether other describes the same generation of the object.
func (i Identity) SameArtifact(other Info) bool {
	return i.Size == other.Size && i.ETag == other.ETag && i.VersionID == other.VersionID
}
