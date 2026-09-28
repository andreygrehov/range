package oci

import (
	"strings"
)

// Reference is an image name resolved to a registry, repository and a tag or
// digest.
type Reference struct {
	registry, repository, reference string
}

// ParseReference resolves an image name the way docker pull does: Docker Hub
// and library/ by default, and a digest winning over a tag.
func ParseReference(ref string) Reference {
	registry, repository := "registry-1.docker.io", ref
	if host, rest, found := strings.Cut(ref, "/"); found &&
		(strings.Contains(host, ".") || strings.Contains(host, ":") || host == "localhost") {
		registry, repository = host, rest
	}
	// docker.io is Docker Hub's name, not its registry API; containerd and
	// podman rewrite it the same way.
	if registry == "docker.io" || registry == "index.docker.io" {
		registry = "registry-1.docker.io"
	}
	// A digest pins the image and wins over any tag next to it; it has to be
	// cut first, because the digest itself contains a colon.
	tag := "latest"
	digest := ""
	if name, after, found := strings.Cut(repository, "@"); found {
		repository, digest = name, after
	}
	if name, candidate, found := strings.Cut(repository, ":"); found && !strings.Contains(candidate, "/") {
		repository, tag = name, candidate
	}
	if digest != "" {
		tag = digest
	}
	if registry == "registry-1.docker.io" && !strings.Contains(repository, "/") {
		repository = "library/" + repository
	}
	return Reference{registry: registry, repository: repository, reference: tag}
}
