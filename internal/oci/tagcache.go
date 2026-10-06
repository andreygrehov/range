package oci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// A tag is resolved over the network once, then from this cache, and checked
// again in the background while the session runs. A moved tag is picked up by
// the next session, the way an image already pulled stays in use with
// docker run. The cached manifest and config are checked against their
// digests every time, so the cache saves round trips and nothing else.
type cachedResolution struct {
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Reference  string `json:"reference"`
	Origin     string `json:"origin,omitempty"` // the registry behind a mirror
	Digest     string `json:"digest"`
	Manifest   []byte `json:"manifest"`
	Config     []byte `json:"config"`
}

// refreshes tracks the background checks, so a test can wait for them.
var refreshes sync.WaitGroup

func resolutionPath(dir, image string, platform Platform) string {
	sum := sha256.Sum256([]byte(image + "|" + platform.String()))
	return filepath.Join(dir, "tags", hex.EncodeToString(sum[:16])+".json")
}

// resolveCached resolves image from the cache when it can, and refreshes the
// cache in the background; otherwise it resolves over the network and fills
// the cache.
func resolveCached(ctx context.Context, dir, image string, platform Platform) (resolved, error) {
	path := resolutionPath(dir, image, platform)
	if r, ok := loadResolution(path, platform); ok {
		if !isDigest(ParseReference(image).reference) {
			refreshes.Add(1)
			go func() {
				defer refreshes.Done()
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				if fresh, err := resolve(ctx, image, platform); err == nil {
					saveResolution(path, fresh)
				}
			}()
		}
		return r, nil
	}
	r, err := resolve(ctx, image, platform)
	if err != nil {
		return r, err
	}
	saveResolution(path, r)
	return r, nil
}

func loadResolution(path string, platform Platform) (resolved, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return resolved{}, false
	}
	var c cachedResolution
	if json.Unmarshal(data, &c) != nil || verifyDigest(c.Manifest, c.Digest) != nil {
		return resolved{}, false
	}
	m, err := parseManifest(c.Manifest)
	if err != nil || len(m.Layers) == 0 {
		return resolved{}, false
	}
	config, err := parseConfig(c.Config, m.Config.Digest)
	if err != nil {
		return resolved{}, false
	}
	if config.Architecture != "" && !platform.matches(config.OS, config.Architecture, config.Variant) {
		return resolved{}, false
	}
	ref := Reference{registry: c.Registry, repository: c.Repository, reference: c.Reference}
	// No token yet: reads ask for one when the registry first refuses them,
	// so a session served from the cache may never need the network.
	client := newClient(ref)
	if c.Origin != "" {
		origin := ref
		origin.registry = c.Origin
		client.fallback = newClient(origin)
	}
	return resolved{client: client, ref: ref, manifest: m, digest: c.Digest, config: config,
		manifestBody: c.Manifest, configBody: c.Config}, true
}

func saveResolution(path string, r resolved) {
	c := cachedResolution{Registry: r.ref.registry, Repository: r.ref.repository, Reference: r.ref.reference,
		Digest: r.digest, Manifest: r.manifestBody, Config: r.configBody}
	if r.client != nil && r.client.fallback != nil {
		c.Origin = r.client.fallback.ref.registry
	}
	data, err := json.Marshal(c)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	tmp.Close()
	os.Rename(tmp.Name(), path)
}
