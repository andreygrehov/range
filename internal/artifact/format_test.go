package artifact

import (
	"context"
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreygrehov/range/internal/erofs"
	"github.com/andreygrehov/range/internal/object"
)

func TestArtifactHeaderRejectsForeignBytes(t *testing.T) {
	if _, err := ParseHeader(make([]byte, headerSize)); err == nil {
		t.Error("an all-zero header was accepted")
	}
	if _, err := ParseHeader([]byte("short")); err == nil {
		t.Error("a truncated header was accepted")
	}
	h := Header{ChunkSize: 1 << 20, LogicalSize: 1 << 30, ChunkCount: 1024}
	buf := h.marshal()
	binary.LittleEndian.PutUint32(buf[8:], 99)
	if _, err := ParseHeader(buf); err == nil {
		t.Error("a future version was accepted")
	}
	back, err := ParseHeader(h.marshal())
	if err != nil || back != h {
		t.Fatalf("header did not survive a round trip: %+v %v", back, err)
	}
}

// The point of chunk alignment: a large file shared by two different trees
// produces the same chunk hashes in both artifacts.
func TestEROFSAlignmentSharesChunksAcrossArtifacts(t *testing.T) {
	const chunk = 1 << 20
	shared := make([]byte, 3<<20)
	rand.New(rand.NewSource(9)).Read(shared)
	build := func(extra string) map[[32]byte]bool {
		root := filepath.Join(t.TempDir(), "root")
		os.MkdirAll(filepath.Join(root, "lib"), 0o755)
		os.WriteFile(filepath.Join(root, "lib", "libshared.so"), shared, 0o755)
		os.WriteFile(filepath.Join(root, extra), []byte(extra+" differs"), 0o644)
		image := filepath.Join(t.TempDir(), "fs.erofs")
		if _, err := erofs.WriteDir(root, image, chunk); err != nil {
			t.Fatal(err)
		}
		artifact := image + ".range"
		if _, err := Pack(image, artifact, chunk); err != nil {
			t.Fatal(err)
		}
		packed, err := Open(context.Background(), object.FileBackend{}, artifact)
		if err != nil || packed == nil {
			t.Fatalf("artifact did not open: %v", err)
		}
		hashes := map[[32]byte]bool{}
		for _, entry := range packed.entries {
			if !entry.Zero() {
				hashes[entry.Hash] = true
			}
		}
		return hashes
	}
	first, second := build("alpha"), build("a-completely-different-name")
	common := 0
	for h := range first {
		if second[h] {
			common++
		}
	}
	if common < 3 {
		t.Fatalf("only %d chunks shared; the 3 MiB file should contribute 3", common)
	}
}
