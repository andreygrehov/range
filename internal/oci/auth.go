package oci

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Range signs in to a registry the way Docker does, from Docker's own
// configuration: after docker login, Range reads private images too, and
// pulls from Docker Hub under the account's rate limit rather than an
// anonymous one. It keeps no credentials of its own, and asks for them only
// from a registry's token service or a registry that wants them directly.
//
// The configuration is $DOCKER_CONFIG/config.json, or ~/.docker/config.json.
// For a registry, Range tries its credential helper ("credHelpers", such as
// ecr-login), then the stored login ("auths"), then the credential store
// ("credsStore", such as osxkeychain or desktop).

// credential is a username and a password or token for one registry.
type credential struct {
	user, secret string
}

var (
	credMu    sync.Mutex
	credCache = map[string]*credential{} // by registry; nil when there is none
)

// credentialsFor returns the credentials Docker's configuration holds for a
// registry, or nil. A helper can be slow, or ask the keychain, so each
// registry is looked up once.
func credentialsFor(registry string) *credential {
	credMu.Lock()
	defer credMu.Unlock()
	if c, ok := credCache[registry]; ok {
		return c
	}
	c := lookupCredentials(registry)
	credCache[registry] = c
	return c
}

type dockerConfig struct {
	Auths map[string]struct {
		Auth string `json:"auth"`
	} `json:"auths"`
	CredHelpers map[string]string `json:"credHelpers"`
	CredsStore  string            `json:"credsStore"`
}

func lookupCredentials(registry string) *credential {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		dir = filepath.Join(home, ".docker")
	}
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil
	}
	var cfg dockerConfig
	if json.Unmarshal(data, &cfg) != nil {
		return nil
	}
	server := registryServer(registry)
	if helper := cfg.CredHelpers[server]; helper != "" {
		return fromHelper(helper, server)
	}
	for key, entry := range cfg.Auths {
		if registryServer(key) != server || entry.Auth == "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			continue
		}
		if user, secret, ok := strings.Cut(string(raw), ":"); ok && user != "" {
			return &credential{user, secret}
		}
	}
	if cfg.CredsStore != "" {
		return fromHelper(cfg.CredsStore, server)
	}
	return nil
}

// registryServer is a registry as Docker's configuration names it: the host,
// and Docker Hub's old index name for Docker Hub.
func registryServer(name string) string {
	name = strings.TrimPrefix(strings.TrimPrefix(name, "https://"), "http://")
	name, _, _ = strings.Cut(name, "/")
	switch name {
	case "registry-1.docker.io", "docker.io", "index.docker.io", "registry.hub.docker.com":
		return "index.docker.io"
	}
	return name
}

// fromHelper asks a Docker credential helper, docker-credential-NAME, for a
// server's credentials. Docker Hub's helpers know it by its old index URL.
func fromHelper(name, server string) *credential {
	if server == "index.docker.io" {
		server = "https://index.docker.io/v1/"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker-credential-"+name, "get")
	cmd.Stdin = strings.NewReader(server)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var reply struct{ Username, Secret string }
	if json.NewDecoder(bytes.NewReader(out)).Decode(&reply) != nil || reply.Secret == "" {
		return nil
	}
	// "<token>" names an identity token, which needs an OAuth exchange that
	// Range does not make: it reads such a registry anonymously.
	if reply.Username == "" || reply.Username == "<token>" {
		return nil
	}
	return &credential{reply.Username, reply.Secret}
}
