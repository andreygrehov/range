package oci

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultCatalog is where Range looks for the index of a layer it has not
// indexed itself. Indexes there are named by layer digest, sharded by its
// first two hex digits: <catalog>/<ab>/sha256-<ab...>.idx. RANGE_INDEX_URL
// points Range at another catalog, and "off" turns the catalog off.
const DefaultCatalog = "https://github.com/andreygrehov/range-index/releases/download"

// catalogURL is the catalog in use, or "" when it is off.
func catalogURL() string {
	switch v := strings.TrimRight(os.Getenv("RANGE_INDEX_URL"), "/"); v {
	case "":
		return DefaultCatalog
	case "off", "none":
		return ""
	default:
		return v
	}
}

// catalogName is a layer index's path in a catalog, or "" for a digest the
// catalog does not hold.
func catalogName(digest string) string {
	hex, ok := strings.CutPrefix(digest, "sha256:")
	if !ok || len(hex) != 64 {
		return ""
	}
	return hex[:2] + "/sha256-" + hex + ".idx"
}

var errNotInCatalog = errors.New("not in the catalog")

// catalogClient fetches indexes. An index is a few megabytes at most, so a
// fixed timeout bounds a catalog that hangs.
func catalogClient() *http.Client {
	return &http.Client{Transport: transport, Timeout: 2 * time.Minute}
}

// fetchIndex downloads a layer's index from the catalog into path. The index
// must name the same digest and size as the manifest, or it is refused: a
// catalog can only save Range the work of building an index, never change
// which bytes it reads.
func fetchIndex(ctx context.Context, digest string, size int64, path string) (*layerIndex, error) {
	base, name := catalogURL(), catalogName(digest)
	if base == "" || name == "" {
		return nil, errNotInCatalog
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := catalogClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotInCatalog
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog: %s", resp.Status)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(temp.Name())
	_, err = io.Copy(temp, io.LimitReader(resp.Body, 1<<30))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, err
	}
	idx, err := loadIndex(temp.Name())
	if err != nil {
		return nil, fmt.Errorf("catalog index: %w", err)
	}
	if idx.Digest != digest || (size > 0 && idx.Size != size) {
		return nil, fmt.Errorf("catalog index is for %s, %d bytes, not %s, %d bytes", idx.Digest, idx.Size, digest, size)
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return nil, err
	}
	return idx, nil
}

// inCatalog reports whether the catalog already holds a layer's index.
func inCatalog(ctx context.Context, digest string) bool {
	base, name := catalogURL(), catalogName(digest)
	if base == "" || name == "" {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, base+"/"+name, nil)
	if err != nil {
		return false
	}
	resp, err := catalogClient().Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// IndexImage builds the index of every layer of image for platform into dir,
// named as the catalog serves them. A layer whose index is already in dir is
// skipped, and so is one already in the catalog when skipPublished is set.
// report receives one line per layer.
func IndexImage(ctx context.Context, image string, platform Platform, dir string, skipPublished bool, report func(string)) error {
	r, err := resolve(ctx, image, platform)
	if err != nil {
		return fmt.Errorf("%s: %w", image, err)
	}
	for i, layer := range r.manifest.Layers {
		name := catalogName(layer.Digest)
		if name == "" {
			report(fmt.Sprintf("%s %s layer %d: %s is not a sha256 digest, skipped", image, platform, i+1, layer.Digest))
			continue
		}
		path := filepath.Join(dir, filepath.FromSlash(name))
		if _, err := os.Stat(path); err == nil {
			report(fmt.Sprintf("%s %s layer %d/%d: already built", image, platform, i+1, len(r.manifest.Layers)))
			continue
		}
		if skipPublished && inCatalog(ctx, layer.Digest) {
			report(fmt.Sprintf("%s %s layer %d/%d: already in the catalog", image, platform, i+1, len(r.manifest.Layers)))
			continue
		}
		started := time.Now()
		idx, err := buildIndex(ctx, r.client, layer.Digest, "")
		if err != nil {
			return fmt.Errorf("%s layer %s: %w", image, layer.Digest, err)
		}
		if err := saveIndex(path, idx); err != nil {
			return err
		}
		info, _ := os.Stat(path)
		report(fmt.Sprintf("%s %s layer %d/%d: %s, %d bytes of layer, %d bytes of index, %.1f s",
			image, platform, i+1, len(r.manifest.Layers), name, layer.Size, info.Size(), time.Since(started).Seconds()))
	}
	return nil
}
