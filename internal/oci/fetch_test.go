package oci

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Layers arrive gzip, zstd or plain; the bytes decide, and a label that
// contradicts them, or names a compression Range cannot read, is an error.
func TestDecompressLayer(t *testing.T) {
	plain := tarLayer(t, tarEntry(t, &tar.Header{Name: "hello", Typeflag: tar.TypeReg, Mode: 0o644}, "hi")).Bytes()
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(plain)
	zw.Close()
	enc, _ := zstd.NewWriter(nil)
	zs := enc.EncodeAll(plain, nil)
	enc.Close()
	const oci = "application/vnd.oci.image.layer.v1.tar"
	for _, tc := range []struct {
		name, mediaType string
		blob            []byte
		ok              bool
	}{
		{"oci plain", oci, plain, true},
		{"oci gzip", oci + "+gzip", gz.Bytes(), true},
		{"oci zstd", oci + "+zstd", zs, true},
		{"docker gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip", gz.Bytes(), true},
		{"no media type, zstd bytes", "", zs, true},
		{"labelled gzip, carries zstd", oci + "+gzip", zs, false},
		{"labelled zstd, carries plain tar", oci + "+zstd", plain, false},
		{"bzip2", oci + "+bzip2", []byte("BZh91AY&SY"), false},
	} {
		stream, done, err := decompressLayer(bytes.NewReader(tc.blob), tc.mediaType)
		if !tc.ok {
			if err == nil {
				t.Errorf("%s: accepted", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		got, err := io.ReadAll(stream)
		done()
		if err != nil || !bytes.Equal(got, plain) {
			t.Errorf("%s: tar stream differs (%v)", tc.name, err)
		}
	}
}

func TestLayerDigestsAreVerifiedNotSkipped(t *testing.T) {
	blob := tarLayer(t, tarEntry(t, &tar.Header{Name: "f", Typeflag: tar.TypeReg, Mode: 0o644}, "x")).Bytes()
	s256, s512 := sha256.Sum256(blob), sha512.Sum512(blob)
	apply := func(r io.Reader) error { _, err := io.Copy(io.Discard, tar.NewReader(r)); return err }
	for _, tc := range []struct {
		digest string
		ok     bool
	}{
		{"sha256:" + hex.EncodeToString(s256[:]), true},
		{"sha512:" + hex.EncodeToString(s512[:]), true},
		{"sha256:" + strings.Repeat("0", 64), false},
		{"sha512:" + strings.Repeat("0", 128), false},
		{"md5:0123", false},
		{"no-algorithm", false},
	} {
		err := applyVerifiedLayer(bytes.NewReader(blob), tc.digest, "application/vnd.oci.image.layer.v1.tar", apply)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.digest, err, tc.ok)
		}
	}
}
