package oci

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dockerConfigDir writes a Docker configuration and points DOCKER_CONFIG at it.
func dockerConfigDir(t *testing.T, config string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dir)
}

func basicAuth(user, secret string) string {
	return base64.StdEncoding.EncodeToString([]byte(user + ":" + secret))
}

func TestCredentialsFromDockerLogin(t *testing.T) {
	dockerConfigDir(t, fmt.Sprintf(`{"auths": {
		"https://index.docker.io/v1/": {"auth": %q},
		"ghcr.io": {"auth": %q}}}`, basicAuth("me", "hub-token"), basicAuth("me", "gh-token")))
	for registry, want := range map[string]string{
		"registry-1.docker.io": "hub-token", "ghcr.io": "gh-token", "quay.io": "",
	} {
		c := lookupCredentials(registry)
		if got := ""; c != nil {
			got = c.secret
			if got != want {
				t.Errorf("%s: secret %q, want %q", registry, got, want)
			}
		} else if want != "" {
			t.Errorf("%s: no credentials, want %q", registry, want)
		}
	}
}

func TestCredentialsFromAHelper(t *testing.T) {
	bin := t.TempDir()
	helper := `#!/bin/sh
read server
case "$server" in
  registry.corp) echo '{"Username": "svc", "Secret": "s3cret"}' ;;
  oauth.corp) echo '{"Username": "<token>", "Secret": "refresh"}' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "docker-credential-corp"), []byte(helper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dockerConfigDir(t, `{"credHelpers": {"registry.corp": "corp", "oauth.corp": "corp"}, "credsStore": "corp"}`)
	if c := lookupCredentials("registry.corp"); c == nil || c.user != "svc" || c.secret != "s3cret" {
		t.Errorf("helper: %+v", c)
	}
	// An identity token needs an OAuth exchange: the registry is read anonymously.
	if c := lookupCredentials("oauth.corp"); c != nil {
		t.Errorf("identity token: %+v", c)
	}
	// The store answers for a registry with no helper of its own, here with nothing.
	if c := lookupCredentials("other.corp"); c != nil {
		t.Errorf("store: %+v", c)
	}
}

// A token service that refuses the credentials still gives a public image an
// anonymous token; one that takes them gives the private token.
func TestTokenWithCredentialsFallsBackToAnonymous(t *testing.T) {
	var sawBasic []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="https://%s/token",service="test"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
		case "/token":
			user, secret, ok := r.BasicAuth()
			sawBasic = append(sawBasic, user)
			switch {
			case ok && secret == "good":
				fmt.Fprint(w, `{"token": "private"}`)
			case ok:
				w.WriteHeader(http.StatusUnauthorized)
			default:
				fmt.Fprint(w, `{"token": "anonymous"}`)
			}
		}
	}))
	defer srv.Close()
	registry := strings.TrimPrefix(srv.URL, "https://")
	for secret, want := range map[string]string{"good": "private", "expired": "anonymous"} {
		credMu.Lock()
		credCache[registry] = &credential{"me", secret}
		credMu.Unlock()
		c := newClient(Reference{registry: registry, repository: "dev"})
		c.client = srv.Client()
		c.authenticate(context.Background())
		if c.token != want {
			t.Errorf("%s password: token %q, want %q (requests: %q)", secret, c.token, want, sawBasic)
		}
	}
	credMu.Lock()
	delete(credCache, registry)
	credMu.Unlock()
}

// A registry that takes credentials directly gets them on every request.
func TestBasicRegistry(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, secret, ok := r.BasicAuth(); !ok || user != "me" || secret != "pw" {
			w.Header().Set("Www-Authenticate", `Basic realm="corp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		fmt.Fprint(w, `{"schemaVersion":2,"layers":[{"digest":"sha256:aa","size":1}]}`)
	}))
	defer srv.Close()
	registry := strings.TrimPrefix(srv.URL, "https://")
	credMu.Lock()
	credCache[registry] = &credential{"me", "pw"}
	credMu.Unlock()
	defer func() { credMu.Lock(); delete(credCache, registry); credMu.Unlock() }()
	c := newClient(Reference{registry: registry, repository: "dev"})
	c.client = srv.Client()
	c.authenticate(context.Background())
	if m, err := c.manifest(context.Background(), "latest"); err != nil || len(m.Layers) != 1 {
		t.Fatalf("manifest from a Basic registry: %v", err)
	}
}

// An expired login whose token service still answers, as ECR's does, gets a
// token the registry then refuses. Range drops the login and reads the
// public image anonymously.
func TestARefusedLoginGivesWayToAnonymous(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
			w.Header().Set("Www-Authenticate", fmt.Sprintf(`Bearer realm="https://%s/token",service="test"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/token":
			if _, _, ok := r.BasicAuth(); ok {
				fmt.Fprint(w, `{"token": "from-an-expired-login"}`)
			} else {
				fmt.Fprint(w, `{"token": "anonymous"}`)
			}
		case r.Header.Get("Authorization") == "Bearer anonymous":
			fmt.Fprint(w, `{"schemaVersion":2,"layers":[{"digest":"sha256:aa","size":1}]}`)
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	}))
	defer srv.Close()
	registry := strings.TrimPrefix(srv.URL, "https://")
	credMu.Lock()
	credCache[registry] = &credential{"AWS", "expired"}
	credMu.Unlock()
	defer func() { credMu.Lock(); delete(credCache, registry); credMu.Unlock() }()
	c := newClient(Reference{registry: registry, repository: "dev"})
	c.client = srv.Client()
	c.authenticate(context.Background())
	if m, err := c.manifest(context.Background(), "latest"); err != nil || len(m.Layers) != 1 {
		t.Fatalf("a public manifest behind an expired login: %v", err)
	}
}
