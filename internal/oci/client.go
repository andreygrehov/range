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
	"time"
)

type client struct {
	client *http.Client
	token  string
	ref    Reference
}

func newClient(ref Reference) *client {
	return &client{client: &http.Client{Timeout: 10 * time.Minute}, ref: ref}
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
		c.token = body.Token
		if c.token == "" {
			c.token = body.AccessToken
		}
	}
}

func (c *client) do(ctx context.Context, method, url string, accept []string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for _, a := range accept {
		req.Header.Add("Accept", a)
	}
	return c.client.Do(req)
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
	url := fmt.Sprintf("https://%s/v2/%s/manifests/%s", c.ref.registry, c.ref.repository, reference)
	resp, err := c.do(ctx, http.MethodGet, url, manifestTypes)
	if err != nil {
		return manifest{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return manifest{}, fmt.Errorf("manifest %s: %s", reference, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return manifest{}, err
	}
	if isDigest(reference) {
		if err := verifyDigest(body, reference); err != nil {
			return manifest{}, fmt.Errorf("manifest %s: %w", reference, err)
		}
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return manifest{}, err
	}
	return m, nil
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
