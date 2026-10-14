package commands

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The catalog signs the map's root, so a mirror or a proxy between a client
// and the catalog cannot make a command run from an image of its choosing.
// The root names every shard by digest, so the root's signature covers the
// whole map.
//
// The signature is the root's last member, "signature": "KEY:SIG", where KEY
// names an Ed25519 key and SIG is base64. It signs the exact bytes of the
// root without that member, so no client re-encodes JSON to check it, and a
// field that a newer catalog adds does not break an older client's check. A
// client that does not check signatures ignores the member.

// signatureMember is how the root's last member starts.
const signatureMember = ",\n  \"signature\": \""

// KeyID names a public key: the first 8 bytes of its SHA-256, in hex.
func KeyID(key ed25519.PublicKey) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:8])
}

// Sign returns root, as Write made it, with its signature as its last member.
func Sign(root []byte, key ed25519.PrivateKey) ([]byte, error) {
	if _, _, ok := splitSignature(root); ok {
		return nil, errors.New("commands: the root is signed already")
	}
	body := bytes.TrimRight(root, "\n")
	if !bytes.HasSuffix(body, []byte("\n}")) {
		return nil, errors.New("commands: the root is not as Write makes it")
	}
	sig := ed25519.Sign(key, root)
	public := key.Public().(ed25519.PublicKey)
	out := append([]byte{}, body[:len(body)-2]...)
	out = append(out, signatureMember...)
	out = append(out, KeyID(public)+":"+base64.StdEncoding.EncodeToString(sig)...)
	return append(out, "\"\n}\n"...), nil
}

// Verify checks a signed root against keys, by ID, and returns the bytes it
// signs.
func Verify(root []byte, keys map[string]ed25519.PublicKey) ([]byte, error) {
	signed, sig, ok := splitSignature(root)
	if !ok {
		return nil, errors.New("commands: the catalog's map is not signed")
	}
	id, encoded, _ := strings.Cut(sig, ":")
	key, known := keys[id]
	if !known {
		return nil, fmt.Errorf("commands: the catalog's map is signed by key %s, which this Range does not trust", id)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || !ed25519.Verify(key, signed, raw) {
		return nil, errors.New("commands: the catalog's map does not match its signature")
	}
	return signed, nil
}

// splitSignature parts a signed root into the bytes it signs and its
// signature.
func splitSignature(root []byte) (signed []byte, sig string, ok bool) {
	at := bytes.LastIndex(root, []byte(signatureMember))
	if at < 0 {
		return nil, "", false
	}
	rest := bytes.TrimRight(root[at+len(signatureMember):], "\n")
	end := bytes.IndexByte(rest, '"')
	if end < 0 || !bytes.Equal(bytes.TrimSpace(rest[end+1:]), []byte("}")) {
		return nil, "", false
	}
	signed = append(append([]byte{}, root[:at]...), "\n}\n"...)
	return signed, string(rest[:end]), true
}

// ParsePublicKey reads a base64 Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("commands: %q is not a base64 Ed25519 public key", s)
	}
	return ed25519.PublicKey(raw), nil
}

// ParsePrivateKey reads a base64 Ed25519 seed.
func ParsePrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(raw) != ed25519.SeedSize {
		return nil, errors.New("commands: the signing key is not a base64 Ed25519 seed")
	}
	return ed25519.NewKeyFromSeed(raw), nil
}
