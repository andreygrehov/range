package oci

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type client struct {
	client *http.Client
	ref    Reference
	// fallback serves the same digests when this registry lacks a blob: the
	// origin behind a mirror. Every blob is checked against its digest, so
	// where it comes from does not matter.
	fallback *client

	mu    sync.Mutex
	token string
}

// transport is how clients reach registries; tests point it at a fake one.
var transport = http.DefaultTransport

func newClient(ref Reference) *client {
	return &client{client: &http.Client{Transport: transport, Timeout: 10 * time.Minute}, ref: ref}
}

// authenticate performs the anonymous token dance registries use for public
// images. A registry that needs no token simply fails here and is used plain.
func (c *client) authenticate(ctx context.Context) {
	// Docker Hub's token service is named differently from its registry host.
	service := c.ref.registry
	realm := "https://auth.docker.io/token"
	if c.ref.registry == "registry-1.docker.io" {
		service = "registry.docker.io"
	} else {
		probe, err := c.do(ctx, http.MethodGet,
			fmt.Sprintf("https://%s/v2/", c.ref.registry), nil)
		if err != nil {
			return
		}
		probe.Body.Close()
		header := probe.Header.Get("Www-Authenticate")
		if !strings.HasPrefix(header, "Bearer ") {
			return
		}
		for _, part := range strings.Split(strings.TrimPrefix(header, "Bearer "), ",") {
			key, value, _ := strings.Cut(strings.TrimSpace(part), "=")
			value = strings.Trim(value, `"`)
			switch key {
			case "realm":
				realm = value
			case "service":
				service = value
			}
		}
	}
	url := fmt.Sprintf("%s?service=%s&scope=repository:%s:pull", realm, service, c.ref.repository)
	resp, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) == nil {
		token := body.Token
		if token == "" {
			token = body.AccessToken
		}
		c.mu.Lock()
		c.token = token
		c.mu.Unlock()
	}
}

func (c *client) do(ctx context.Context, method, url string, accept []string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	return c.send(req)
}

// send adds the bearer token, if there is one. Go drops it when a registry
// redirects a blob to another host, so it never reaches a CDN.
func (c *client) send(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	token := c.token
	c.mu.Unlock()
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return c.client.Do(req)
}

// blobStream opens a whole blob for reading. A layer can take longer to read
// than any fixed timeout on a slow link, so only the context bounds it, and a
// token that expired while an earlier layer was read is renewed once. A
// connection the registry or its CDN drops half way is picked up where it
// stopped, a few times, so one reset does not cost the whole layer.
func (c *client) blobStream(ctx context.Context, digest string) (io.ReadCloser, error) {
	b := &resumingBlob{c: c, ctx: ctx, digest: digest}
	if err := b.open(); err != nil {
		return nil, err
	}
	return b, nil
}

// blobResumes bounds how often one blob read picks up after a drop.
const blobResumes = 3

// resumingBlob reads a blob front to back, resuming with a ranged request
// from the byte it reached when the connection fails.
type resumingBlob struct {
	c       *client
	ctx     context.Context
	digest  string
	body    io.ReadCloser
	pos     int64
	resumes int
}

func (b *resumingBlob) open() error {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", b.c.ref.registry, b.c.ref.repository, b.digest)
	stream := &http.Client{Transport: b.c.client.Transport}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if b.pos > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", b.pos))
		}
		b.c.mu.Lock()
		token := b.c.token
		b.c.mu.Unlock()
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := stream.Do(req)
		if err != nil {
			return fmt.Errorf("blob %s: %w", b.digest, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			b.c.authenticate(b.ctx)
			continue
		}
		if resp.StatusCode == http.StatusNotFound && b.c.fallback != nil {
			resp.Body.Close()
			b.c, attempt = b.c.fallback, -1
			url = fmt.Sprintf("https://%s/v2/%s/blobs/%s", b.c.ref.registry, b.c.ref.repository, b.digest)
			continue
		}
		want := http.StatusOK
		if b.pos > 0 {
			want = http.StatusPartialContent
		}
		if resp.StatusCode != want {
			resp.Body.Close()
			return fmt.Errorf("blob %s: %s", b.digest, resp.Status)
		}
		if b.pos > 0 && !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", b.pos)) {
			resp.Body.Close()
			return fmt.Errorf("blob %s: resumed at the wrong offset: %q", b.digest, resp.Header.Get("Content-Range"))
		}
		b.body = resp.Body
		return nil
	}
}

func (b *resumingBlob) Read(p []byte) (int, error) {
	for {
		n, err := b.body.Read(p)
		b.pos += int64(n)
		if err == nil || err == io.EOF || b.ctx.Err() != nil || b.resumes >= blobResumes {
			return n, err
		}
		b.resumes++
		b.body.Close()
		if openErr := b.open(); openErr != nil {
			return n, fmt.Errorf("blob %s: %w, and resuming failed: %v", b.digest, err, openErr)
		}
		if n > 0 {
			return n, nil
		}
	}
}

func (b *resumingBlob) Close() error { return b.body.Close() }

// blobRange reads length bytes of a blob from offset. Anonymous tokens last
// minutes and a session lasts as long as someone keeps a shell open, so a
// refused request gets a fresh token and one more try.
func (c *client) blobRange(ctx context.Context, digest string, offset, length int64) ([]byte, error) {
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", c.ref.registry, c.ref.repository, digest)
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
		resp, err := c.send(req)
		if err != nil {
			return nil, fmt.Errorf("blob %s: %w", digest, err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			c.authenticate(ctx)
			continue
		}
		if resp.StatusCode == http.StatusNotFound && c.fallback != nil {
			resp.Body.Close()
			return c.fallback.blobRange(ctx, digest, offset, length)
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusPartialContent:
		case http.StatusOK:
			// A server that ignores Range sends the whole blob; skip to the part asked for.
			if _, err := io.CopyN(io.Discard, resp.Body, offset); err != nil {
				return nil, fmt.Errorf("blob %s: %w", digest, err)
			}
		default:
			return nil, fmt.Errorf("blob %s: range %d+%d: %s", digest, offset, length, resp.Status)
		}
		data := make([]byte, length)
		if _, err := io.ReadFull(resp.Body, data); err != nil {
			return nil, fmt.Errorf("blob %s: short read: %w", digest, err)
		}
		return data, nil
	}
}

var manifestTypes = []string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}

type manifest struct {
	MediaType string `json:"mediaType"`
	Config    struct {
		Digest string `json:"digest"`
	} `json:"config"`
	Manifests []struct {
		Digest   string `json:"digest"`
		Platform struct {
			Architecture string `json:"architecture"`
			OS           string `json:"os"`
			Variant      string `json:"variant"`
		} `json:"platform"`
	} `json:"manifests"`
	Layers []struct {
		Digest    string `json:"digest"`
		MediaType string `json:"mediaType"`
		Size      int64  `json:"size"`
	} `json:"layers"`
}

// manifest fetches a manifest or index. When it is asked for by digest — an
// image@sha256:... reference, or a platform manifest named by an index — the
// body is checked against that digest, because every layer digest checked
// later is only as trustworthy as the manifest that listed it.
func (c *client) manifest(ctx context.Context, reference string) (manifest, error) {
	m, _, err := c.manifestWithDigest(ctx, reference)
	return m, err
}

// manifestWithDigest also returns the manifest's digest: the one asked for,
// or the SHA-256 of the body when it was asked for by tag.
func (c *client) manifestWithDigest(ctx context.Context, reference string) (manifest, string, error) {
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", c.ref.registry, c.ref.repository, reference)
	resp, err := c.do(ctx, http.MethodGet, url, manifestTypes)
	if err != nil {
		return manifest{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return manifest{}, "", fmt.Errorf("manifest %s: %s", reference, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return manifest{}, "", err
	}
	if isDigest(reference) {
		if err := verifyDigest(body, reference); err != nil {
			return manifest{}, "", fmt.Errorf("manifest %s: %w", reference, err)
		}
	}
	digest := reference
	if !isDigest(reference) {
		sum := sha256.Sum256(body)
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return manifest{}, "", err
	}
	return m, digest, nil
}

// isDigest: a tag cannot contain a colon, so anything with one is a digest,
// and one in an algorithm Range cannot check is refused, not treated as a tag.
func isDigest(reference string) bool { return strings.Contains(reference, ":") }

// newDigestHash returns the hash a digest names, or an error for one Range
// cannot verify, so an unknown algorithm is refused rather than skipped.
func newDigestHash(digest string) (hash.Hash, string, error) {
	algo, want, _ := strings.Cut(digest, ":")
	switch algo {
	case "sha256":
		return sha256.New(), want, nil
	case "sha512":
		return sha512.New(), want, nil
	}
	return nil, "", fmt.Errorf("digest %q uses an algorithm range cannot verify", digest)
}

func verifyDigest(body []byte, digest string) error {
	sum, want, err := newDigestHash(digest)
	if err != nil {
		return err
	}
	sum.Write(body)
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return fmt.Errorf("digest mismatch: got %s", got)
	}
	return nil
}

// ImageConfig is the part of an image's config that describes how to run in it.
type ImageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Variant      string `json:"variant"`
	Config       struct {
		Env        []string `json:"Env"`
		WorkingDir string   `json:"WorkingDir"`
		Entrypoint []string `json:"Entrypoint"`
		Cmd        []string `json:"Cmd"`
	} `json:"config"`
}

func (c *client) imageConfig(ctx context.Context, ref Reference, digest string) (ImageConfig, error) {
	var config ImageConfig
	if digest == "" {
		return config, errors.New("the manifest names no image config")
	}
	url := fmt.Sprintf("https://%s/v2/%s/blobs/%s", ref.registry, ref.repository, digest)
	resp, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return config, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return config, fmt.Errorf("image config %s: %s", digest, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return config, err
	}
	if err := verifyDigest(body, digest); err != nil {
		return config, fmt.Errorf("image config %s: %w", digest, err)
	}
	return config, json.Unmarshal(body, &config)
}
