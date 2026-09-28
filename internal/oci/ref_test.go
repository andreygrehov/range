package oci

import (
	"testing"
)

func TestParseOCIRef(t *testing.T) {
	tests := []struct {
		in                        string
		registry, repo, reference string
	}{
		{"ubuntu:24.04", "registry-1.docker.io", "library/ubuntu", "24.04"},
		{"ubuntu", "registry-1.docker.io", "library/ubuntu", "latest"},
		{"bitnami/golang:1.22", "registry-1.docker.io", "bitnami/golang", "1.22"},
		{"ghcr.io/acme/dev:v2", "ghcr.io", "acme/dev", "v2"},
		{"public.ecr.aws/lts/ubuntu:24.04", "public.ecr.aws", "lts/ubuntu", "24.04"},
		// The digest contains a colon, so it has to be cut before the tag.
		{"alpine@sha256:beef", "registry-1.docker.io", "library/alpine", "sha256:beef"},
		{"alpine:3@sha256:beef", "registry-1.docker.io", "library/alpine", "sha256:beef"},
		{"localhost:5000/dev@sha512:cafe", "localhost:5000", "dev", "sha512:cafe"},
		// Fully qualified Docker Hub names are what people paste.
		{"docker.io/library/golang:1.23", "registry-1.docker.io", "library/golang", "1.23"},
		{"docker.io/golang:1.23", "registry-1.docker.io", "library/golang", "1.23"},
		{"index.docker.io/bitnami/golang", "registry-1.docker.io", "bitnami/golang", "latest"},
	}
	for _, tc := range tests {
		got := ParseReference(tc.in)
		if got.registry != tc.registry || got.repository != tc.repo || got.reference != tc.reference {
			t.Errorf("parseOCIRef(%q) = %+v, want %s %s %s", tc.in, got, tc.registry, tc.repo, tc.reference)
		}
	}
}
