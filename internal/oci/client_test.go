package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A manifest asked for by digest must hash to it: every layer digest checked
// later came out of that manifest, so an unchecked one makes them all moot.
func TestManifestIsVerifiedAgainstItsDigest(t *testing.T) {
	good := []byte(`{"schemaVersion":2,"layers":[{"digest":"sha256:aa","size":1}]}`)
	sum := sha256.Sum256(good)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	tampered := []byte(`{"schemaVersion":2,"layers":[{"digest":"sha256:bb","size":1}]}`)
	serve := good
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(serve)
	}))
	defer srv.Close()
	client := newClient(Reference{registry: strings.TrimPrefix(srv.URL, "https://"), repository: "dev"})
	client.client = srv.Client()
	if m, err := client.manifest(context.Background(), digest); err != nil || len(m.Layers) != 1 {
		t.Fatalf("a matching manifest was refused: %v", err)
	}
	serve = tampered
	if _, err := client.manifest(context.Background(), digest); err == nil {
		t.Fatal("a manifest that does not match its digest was accepted")
	}
	if _, err := client.manifest(context.Background(), "md5:0123"); err == nil {
		t.Fatal("a digest in an unknown algorithm was accepted")
	}
	// A tag names nothing to check against; it is resolved as served.
	if _, err := client.manifest(context.Background(), "latest"); err != nil {
		t.Fatalf("a tag lookup failed: %v", err)
	}
}

// A mirror can hold an image's manifest and still lack one of its layers.
// The layer then comes from the registry behind the mirror, whole or by range.
func TestABlobTheMirrorLacksComesFromTheOrigin(t *testing.T) {
	blob := []byte("a layer only the origin has")
	sum := sha256.Sum256(blob)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	mirror := httptest.NewTLSServer(http.NotFoundHandler())
	defer mirror.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/dev/blobs/"+digest {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(blob))
	}))
	defer origin.Close()
	old := transport
	transport = origin.Client().Transport // one test certificate serves both
	defer func() { transport = old }()
	c := newClient(Reference{registry: strings.TrimPrefix(mirror.URL, "https://"), repository: "dev"})
	c.fallback = newClient(Reference{registry: strings.TrimPrefix(origin.URL, "https://"), repository: "dev"})

	part, err := c.blobRange(context.Background(), digest, 2, 5)
	if err != nil || string(part) != "layer" {
		t.Fatalf("range read = %q, %v", part, err)
	}
	stream, err := c.blobStream(context.Background(), digest)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := io.ReadAll(stream)
	stream.Close()
	if err != nil || !bytes.Equal(whole, blob) {
		t.Fatalf("whole read = %q, %v", whole, err)
	}

	c.fallback = nil
	if _, err := c.blobRange(context.Background(), digest, 0, 1); err == nil {
		t.Fatal("a missing blob was read with nowhere to fall back to")
	}
}
