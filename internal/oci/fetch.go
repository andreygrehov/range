package oci

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/andreygrehov/range/internal/bytesize"
)

// FetchRootfs materialises an image's filesystem into dir.
func FetchRootfs(ctx context.Context, image, dir string, platform Platform) error {
	config, err := fetch(ctx, image, platform, func(layer io.Reader, _ int) error {
		return extractLayer(layer, dir)
	})
	if err != nil {
		return err
	}
	return WriteEnvironment(dir, image, config, platform)
}

// fetch resolves an image for this platform and hands each layer, digest
// verified, to apply in order. Where the layers go is the caller's business:
// a directory for the ext4 path, an in-memory tree for EROFS.
func fetch(ctx context.Context, image string, want Platform, apply func(layer io.Reader, index int) error) (ImageConfig, error) {
	img, err := resolve(ctx, image, want)
	if err != nil {
		return ImageConfig{}, err
	}
	none, config, client, ref, manifest := ImageConfig{}, img.config, img.client, img.ref, img.manifest
	for index, layer := range manifest.Layers {
		fmt.Fprintf(os.Stderr, "  layer %d/%d  %s\n", index+1, len(manifest.Layers), bytesize.Format(layer.Size))
		url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.registry, ref.repository, layer.Digest)
		resp, err := client.do(ctx, http.MethodGet, url, nil)
		if err != nil {
			return none, err
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return none, fmt.Errorf("blob %s: %s", layer.Digest, resp.Status)
		}
		err = applyVerifiedLayer(resp.Body, layer.Digest, layer.MediaType, func(tarStream io.Reader) error {
			return apply(tarStream, index)
		})
		resp.Body.Close()
		if err != nil {
			return none, fmt.Errorf("layer %s: %w", layer.Digest, err)
		}
	}
	return config, nil
}

// applyVerifiedLayer decompresses one layer blob for apply and checks it
// against its digest. The digest covers the blob as served, so the hash is
// taken before decompression and the remainder drained once the tar ends. A
// digest this code cannot check is refused rather than skipped.
func applyVerifiedLayer(blob io.Reader, digest, mediaType string, apply func(io.Reader) error) error {
	sum, want, err := newDigestHash(digest)
	if err != nil {
		return err
	}
	algo, _, _ := strings.Cut(digest, ":")
	source := io.TeeReader(blob, sum)
	tarStream, closeLayer, err := decompressLayer(source, mediaType)
	if err != nil {
		return err
	}
	err = apply(tarStream)
	closeLayer()
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, source); err != nil {
		return err
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("digest mismatch: got %s:%s", algo, got)
	}
	return nil
}

// decompressLayer turns a layer blob into its tar stream. The compression is
// read from the blob's own magic bytes, so a registry that labels a layer
// loosely still works, and a media type that names a compression the bytes do
// not carry, or one this code does not speak, is an error rather than a tar
// parser failing somewhere confusing.
func decompressLayer(blob io.Reader, mediaType string) (io.Reader, func(), error) {
	buffered := bufio.NewReader(blob)
	magic, _ := buffered.Peek(4)
	isGzip := len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b
	isZstd := bytes.Equal(magic, []byte{0x28, 0xb5, 0x2f, 0xfd})
	declared := ""
	if _, suffix, found := strings.Cut(mediaType, ".tar"); found {
		declared = strings.TrimPrefix(strings.TrimPrefix(suffix, "+"), ".")
	}
	switch {
	case isGzip && (declared == "" || declared == "gzip"):
		zr, err := gzip.NewReader(buffered)
		if err != nil {
			return nil, nil, err
		}
		return zr, func() { zr.Close() }, nil
	case isZstd && (declared == "" || declared == "zstd"):
		zr, err := zstd.NewReader(buffered, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}
		return zr, zr.Close, nil
	case !isGzip && !isZstd && declared == "":
		return buffered, func() {}, nil
	}
	return nil, nil, fmt.Errorf("layer media type %q does not match its contents, or uses a compression range cannot read", mediaType)
}

// resolved is an image pinned to one platform's manifest.
type resolved struct {
	client   *client
	ref      Reference
	manifest manifest
	digest   string // of the platform manifest
	config   ImageConfig

	// The platform manifest and the config as served, for the tag cache.
	manifestBody, configBody []byte
}

// resolve finds the manifest for want, following an index, and reads the
// image config. Every digest below it is only as good as this manifest, which
// is checked against its digest whenever one is known.
func resolve(ctx context.Context, image string, want Platform) (resolved, error) {
	ref := ParseReference(image)
	// RANGE_REGISTRY_MIRROR serves Docker Hub images through a mirror with the
	// same digests, such as mirror.gcr.io, away from Docker Hub's rate limit.
	// A mirror that cannot serve the image is passed over for Docker Hub, and
	// a layer the mirror lacks is read from Docker Hub.
	if mirror := os.Getenv("RANGE_REGISTRY_MIRROR"); mirror != "" && ref.registry == "registry-1.docker.io" {
		mirrored := ref
		mirrored.registry = strings.TrimSuffix(strings.TrimPrefix(mirror, "https://"), "/")
		if r, err := resolveFrom(ctx, image, mirrored, want); err == nil {
			r.client.fallback = newClient(ref)
			return r, nil
		}
	}
	return resolveFrom(ctx, image, ref, want)
}

func resolveFrom(ctx context.Context, image string, ref Reference, want Platform) (resolved, error) {
	client := newClient(ref)
	client.authenticate(ctx)

	manifest, digest, body, err := client.manifestWithDigest(ctx, ref.reference)
	if err != nil {
		return resolved{}, err
	}
	if len(manifest.Manifests) > 0 { // multi-platform index
		chosen := ""
		for _, entry := range manifest.Manifests {
			if want.matches(entry.Platform.OS, entry.Platform.Architecture, entry.Platform.Variant) {
				chosen = entry.Digest
				break
			}
		}
		if chosen == "" {
			return resolved{}, fmt.Errorf("%s has no %s image", image, want)
		}
		if manifest, digest, body, err = client.manifestWithDigest(ctx, chosen); err != nil {
			return resolved{}, err
		}
	}
	if len(manifest.Layers) == 0 {
		return resolved{}, fmt.Errorf("%s has no layers", image)
	}
	config, configBody, err := client.imageConfig(ctx, ref, manifest.Config.Digest)
	if err != nil {
		return resolved{}, err
	}
	// A single-platform image has no index to choose from; its config says
	// what it was built for.
	if config.Architecture != "" && !want.matches(config.OS, config.Architecture, config.Variant) {
		return resolved{}, fmt.Errorf("%s is a %s/%s image, not %s; pick a tag built for %s",
			image, config.OS, config.Architecture, want, want)
	}
	return resolved{client: client, ref: ref, manifest: manifest, digest: digest, config: config,
		manifestBody: body, configBody: configBody}, nil
}
