package artifact

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"testing"

	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/rangetest"
)

func TestArtifactRoundTrip(t *testing.T) {
	const chunk = 64 << 10
	source, want := rangetest.ArtifactSource(t, chunk)
	target := source + ".range"
	st, err := Pack(source, target, chunk)
	if err != nil {
		t.Fatalf("packArtifact: %v", err)
	}
	if st.Logical != int64(len(want)) {
		t.Fatalf("logical = %d, want %d", st.Logical, len(want))
	}
	if st.Stored >= st.Logical {
		t.Errorf("stored %d is not smaller than logical %d", st.Stored, st.Logical)
	}

	packed, err := Open(context.Background(), object.FileBackend{}, target)
	if err != nil || packed == nil {
		t.Fatalf("openArtifact: %v", err)
	}
	info, err := packed.Stat(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != int64(len(want)) {
		t.Fatalf("stat size = %d, want %d", info.Size, len(want))
	}
	// Every read, including ones straddling chunk boundaries and zero runs,
	// must return exactly the original bytes.
	rng := rand.New(rand.NewSource(5))
	for i := 0; i < 200; i++ {
		off := rng.Int63n(int64(len(want)))
		length := rng.Int63n(int64(len(want))-off) + 1
		got, err := packed.ReadRange(context.Background(), target, off, length, "", "")
		if err != nil {
			t.Fatalf("readRange(%d,%d): %v", off, length, err)
		}
		if !bytes.Equal(got, want[off:off+length]) {
			t.Fatalf("readRange(%d,%d) returned different bytes", off, length)
		}
	}
}

// A short response must be an error, not a slice past the end of the buffer.
func TestArtifactRejectsShortResponses(t *testing.T) {
	const chunk = 64 << 10
	source, _ := rangetest.ArtifactSource(t, chunk)
	target := source + ".range"
	if _, err := Pack(source, target, chunk); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), shortBackend{}, target); err == nil {
		t.Fatal("a short index read was accepted")
	}
	packed, err := Open(context.Background(), object.FileBackend{}, target)
	if err != nil {
		t.Fatal(err)
	}
	packed.inner = shortBackend{}
	for i, e := range packed.entries {
		if e.Zero() {
			continue
		}
		_, err := packed.ReadRange(context.Background(), target, int64(i)*chunk, 1, "", "")
		if !errors.Is(err, errChunkCorrupt) {
			t.Fatalf("short chunk read = %v, want errChunkCorrupt", err)
		}
		return
	}
	t.Fatal("no stored chunk to read")
}

// A chunk whose bytes do not match the hash the index recorded must never be
// served: it would put silent corruption inside a mounted filesystem.
func TestArtifactRejectsCorruptChunk(t *testing.T) {
	const chunk = 64 << 10
	source, want := rangetest.ArtifactSource(t, chunk)
	target := source + ".range"
	if _, err := Pack(source, target, chunk); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	packed, err := Open(ctx, object.FileBackend{}, target)
	if err != nil || packed == nil {
		t.Fatalf("open artifact: %v", err)
	}
	// Chunk 2 is compressible, so it is stored compressed. Flip a byte inside
	// its stored bytes, which the index hash covers but the container does not.
	entry := packed.entries[2]
	if entry.Zero() {
		t.Fatal("test expects chunk 2 to be stored")
	}
	file, err := os.OpenFile(target, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	at := entry.Offset + int64(entry.Length) - 1
	one := make([]byte, 1)
	if _, err := file.ReadAt(one, at); err != nil {
		t.Fatal(err)
	}
	one[0] ^= 0xff
	if _, err := file.WriteAt(one, at); err != nil {
		t.Fatal(err)
	}
	file.Close()

	fresh, err := Open(ctx, object.FileBackend{}, target)
	if err != nil || fresh == nil {
		t.Fatalf("reopen artifact: %v", err)
	}
	_, err = fresh.ReadRange(ctx, target, 2*chunk, chunk, "", "")
	if err == nil {
		t.Fatal("a corrupted chunk was served as if it were good")
	}
	if !errors.Is(err, errChunkCorrupt) {
		t.Fatalf("error = %v, want it to report a chunk mismatch", err)
	}
	// Untouched chunks still read correctly.
	good, err := fresh.ReadRange(ctx, target, 0, chunk, "", "")
	if err != nil {
		t.Fatalf("an untouched chunk stopped working: %v", err)
	}
	if !bytes.Equal(good, want[:chunk]) {
		t.Fatal("an untouched chunk returned different bytes")
	}
}

// The hash check has to cover chunks stored uncompressed too.
func TestArtifactVerifiesRawStoredChunks(t *testing.T) {
	const chunk = 64 << 10
	source, _ := rangetest.ArtifactSource(t, chunk)
	target := source + ".range"
	if _, err := Pack(source, target, chunk); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	packed, _ := Open(ctx, object.FileBackend{}, target)
	index := -1
	for i, e := range packed.entries {
		if e.Flags&flagRaw != 0 {
			index = i
			break
		}
	}
	if index < 0 {
		t.Skip("no incompressible chunk in this fixture")
	}
	file, _ := os.OpenFile(target, os.O_RDWR, 0)
	one := []byte{0x5a}
	file.WriteAt(one, packed.entries[index].Offset)
	file.Close()
	fresh, _ := Open(ctx, object.FileBackend{}, target)
	if _, err := fresh.ReadRange(ctx, target, int64(index)*chunk, chunk, "", ""); !errors.Is(err, errChunkCorrupt) {
		t.Fatalf("a corrupted uncompressed chunk was served: %v", err)
	}
}
