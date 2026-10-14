package commands

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) (ed25519.PrivateKey, map[string]ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, map[string]ed25519.PublicKey{KeyID(pub): pub}
}

// signedCatalog writes the test map to a directory as the catalog serves it,
// its root built at built and signed with key.
func signedCatalog(t *testing.T, key ed25519.PrivateKey, built time.Time) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := Write(dir, testMap(), 2, built); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, RootName)
	root, _ := os.ReadFile(path)
	signed, err := Sign(root, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, signed, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSignAndVerify(t *testing.T) {
	key, keys := newKey(t)
	dir := t.TempDir()
	Write(dir, testMap(), 2, time.Unix(1000, 0))
	root, _ := os.ReadFile(filepath.Join(dir, RootName))
	signed, err := Sign(root, key)
	if err != nil {
		t.Fatal(err)
	}
	// Still a root, for a client that ignores the signature.
	var r Root
	if err := json.Unmarshal(signed, &r); err != nil || r.Digits != 2 {
		t.Fatalf("a signed root is no longer a root: %v", err)
	}
	if body, err := Verify(signed, keys); err != nil || !bytes.Equal(body, root) {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := Sign(signed, key); err == nil {
		t.Error("a root was signed twice")
	}

	tampered := bytes.Replace(signed, []byte(`"digits": 2`), []byte(`"digits": 3`), 1)
	if _, err := Verify(tampered, keys); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a changed root: %v", err)
	}
	_, other := newKey(t)
	if _, err := Verify(signed, other); err == nil || !strings.Contains(err.Error(), "does not trust") {
		t.Errorf("another key: %v", err)
	}
	if _, err := Verify(root, keys); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("an unsigned root: %v", err)
	}
}

func TestClientChecksTheMap(t *testing.T) {
	key, keys := newKey(t)
	now := time.Now()
	client := func(base string) *Client {
		return &Client{Base: base, Dir: t.TempDir(), MaxAge: time.Hour, MaxStale: 30 * 24 * time.Hour,
			Keys: keys, HTTP: nil, Now: func() time.Time { return now }}
	}
	ctx := context.Background()

	s, _ := serve(t, signedCatalog(t, key, now.Add(-time.Hour)))
	c := client(s.URL)
	c.HTTP = s.Client()
	if _, err := c.Lookup(ctx, "pdflatex"); err != nil {
		t.Fatalf("a signed map: %v", err)
	}

	unsigned := t.TempDir()
	Write(unsigned, testMap(), 2, now)
	s2, _ := serve(t, unsigned)
	c2 := client(s2.URL)
	c2.HTTP = s2.Client()
	if _, err := c2.Lookup(ctx, "pdflatex"); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Errorf("an unsigned map: %v", err)
	}

	// A mirror hands back an older map than the one this client has: the
	// client keeps its own.
	older, _ := serve(t, signedCatalog(t, key, now.Add(-48*time.Hour)))
	c.Base, c.HTTP = older.URL, older.Client()
	now = now.Add(2 * time.Hour) // past MaxAge, so the client asks again
	root, err := c.root(ctx)
	if err != nil || root.Built.Before(now.Add(-4*time.Hour)) {
		t.Errorf("an older map replaced the one kept: built %s, %v", root.Built, err)
	}

	// A map built long ago is not used.
	stale, _ := serve(t, signedCatalog(t, key, now.Add(-60*24*time.Hour)))
	c3 := client(stale.URL)
	c3.HTTP = stale.Client()
	if _, err := c3.Lookup(ctx, "pdflatex"); err == nil || !strings.Contains(err.Error(), "days old") {
		t.Errorf("a stale map: %v", err)
	}
}
