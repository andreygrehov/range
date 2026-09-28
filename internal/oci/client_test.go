package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
