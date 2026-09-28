package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/rangetest"
)

// A sparse filesystem must not become a dense object.
func TestArtifactDropsZeroChunks(t *testing.T) {
	const chunk = 64 << 10
	path := filepath.Join(t.TempDir(), "sparse.img")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(64 << 20); err != nil { // 64 MiB of nothing
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("only this"), 1<<20); err != nil {
		t.Fatal(err)
	}
	file.Close()
	st, err := Pack(path, path+".range", chunk)
	if err != nil {
		t.Fatal(err)
	}
	if st.Zero < st.Chunks-4 {
		t.Errorf("zero chunks = %d of %d, want nearly all", st.Zero, st.Chunks)
	}
	if st.Stored > st.Logical/100 {
		t.Errorf("stored %d bytes for a 64 MiB sparse file; the zeroes were written", st.Stored)
	}
	packed, err := Open(context.Background(), object.FileBackend{}, path+".range")
	if err != nil || packed == nil {
		t.Fatalf("open sparse artifact: %v", err)
	}
	got, err := packed.ReadRange(context.Background(), path+".range", 1<<20, 9, "", "")
	if err != nil || string(got) != "only this" {
		t.Fatalf("read of the one written region = %q, %v", got, err)
	}
	// A read inside a hole is zeroes, and costs no request at all.
	before := packed.CompressedReqs.Load()
	hole, err := packed.ReadRange(context.Background(), path+".range", 32<<20, 4096, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !allZero(hole) {
		t.Error("a hole did not read back as zeroes")
	}
	if packed.CompressedReqs.Load() != before {
		t.Error("reading a hole issued a remote request")
	}
}

func TestArtifactDeduplicatesIdenticalChunks(t *testing.T) {
	const chunk = 64 << 10
	source, _ := rangetest.ArtifactSource(t, chunk)
	st, err := Pack(source, source+".range", chunk)
	if err != nil {
		t.Fatal(err)
	}
	if st.Deduped < 1 {
		t.Errorf("deduped = %d, want at least the repeated chunk", st.Deduped)
	}
}

func TestPackRefusesChunkSizesNoReaderAccepts(t *testing.T) {
	source, _ := rangetest.ArtifactSource(t, 64<<10)
	for _, size := range []int64{0, 512, 128 << 20} {
		if _, err := Pack(source, source+".range", size); err == nil {
			t.Errorf("chunk size %d was accepted", size)
		}
	}
}
