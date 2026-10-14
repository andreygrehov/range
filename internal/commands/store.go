package commands

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The map is stored as a root and shards. A command's shard is picked by the
// first Digits hex digits of the SHA-256 of its name. A shard is a gzipped
// JSON object from command to owners, and its file is named by the SHA-256
// of its content, so it never changes: a client keeps it forever and checks
// it against its name. The root names the shard for each prefix, and is the
// only file that changes. A client therefore sees one whole version of the
// map or another, never a mix of two.
//
// The catalog lays the shards out as its layer indexes: <ab>/sha256-<ab...>,
// each in the release of its first two hex digits. The root is
// commands/commands.json.

// Format is the version of the layout that this Range reads. A root of a
// newer format asks for a newer Range.
const Format = 1

// Root names every shard of one version of the map.
type Root struct {
	Format int       `json:"format"`
	Built  time.Time `json:"built"`
	// Digits is how many hex digits of a command's SHA-256 pick its shard:
	// 2 makes 256 shards, 3 makes 4096. A larger catalog raises it, and no
	// client changes.
	Digits int `json:"digits"`
	// Shards maps a prefix to its shard's digest. A prefix with no
	// commands has none.
	Shards map[string]string `json:"shards"`
}

// RootName is the root's path in a catalog.
const RootName = "commands/commands.json"

// shardPath is a shard's path in a catalog, or in a directory written to it.
func shardPath(digest string) string {
	hex := digest[len("sha256:"):]
	return hex[:2] + "/sha256-" + hex + ".commands.json.gz"
}

// prefixOf is the prefix that picks a command's shard.
func prefixOf(command string, digits int) string {
	sum := sha256.Sum256([]byte(command))
	return hex.EncodeToString(sum[:])[:digits]
}

// Write lays the map out in dir as the catalog serves it, and returns its
// root. The same map always makes the same shards: one that did not change
// keeps its name, and need not be uploaded again.
func Write(dir string, m Map, digits int, built time.Time) (Root, error) {
	if digits < 1 || digits > 8 {
		return Root{}, fmt.Errorf("commands: %d digits picks no sensible number of shards", digits)
	}
	shards := map[string]Map{}
	for command, owners := range m {
		p := prefixOf(command, digits)
		if shards[p] == nil {
			shards[p] = Map{}
		}
		shards[p][command] = owners
	}
	root := Root{Format: Format, Built: built.UTC(), Digits: digits, Shards: map[string]string{}}
	prefixes := make([]string, 0, len(shards))
	for p := range shards {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, p := range prefixes {
		data, err := encodeShard(shards[p])
		if err != nil {
			return Root{}, err
		}
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if err := writeFile(filepath.Join(dir, shardPath(digest)), data); err != nil {
			return Root{}, err
		}
		root.Shards[p] = digest
	}
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return Root{}, err
	}
	return root, writeFile(filepath.Join(dir, RootName), append(data, '\n'))
}

// encodeShard is a shard's bytes: JSON with its keys sorted, gzipped with no
// name or time, so equal shards are equal bytes.
func encodeShard(m Map) ([]byte, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := gz.Write(data); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeShard reads a shard, after checking it is the one its digest names.
func decodeShard(data []byte, digest string) (Map, error) {
	sum := sha256.Sum256(data)
	if "sha256:"+hex.EncodeToString(sum[:]) != digest {
		return nil, fmt.Errorf("commands: a shard does not match its digest %s", digest)
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var m Map
	if err := json.NewDecoder(gz).Decode(&m); err != nil {
		return nil, fmt.Errorf("commands: shard %s: %w", digest, err)
	}
	return m, nil
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
