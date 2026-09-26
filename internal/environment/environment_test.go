package environment

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvironmentMetadataDefaults(t *testing.T) {
	// No metadata file at all.
	meta := ReadMetadata(t.TempDir())
	if meta.Shell != "/bin/sh" || meta.Workdir != "/" || meta.Hostname != "range" {
		t.Fatalf("defaults = %+v", meta)
	}

	lower := t.TempDir()
	if err := os.MkdirAll(filepath.Join(lower, "etc", "range"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(lower, "etc", "range", "environment.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("full metadata", func(t *testing.T) {
		write(`{"version":1,"name":"acme-dev","shell":"/bin/bash","workdir":"/workspace/acme",
			"hostname":"range","environment":{"GOPATH":"/root/go"}}`)
		meta := ReadMetadata(lower)
		if meta.Name != "acme-dev" || meta.Shell != "/bin/bash" || meta.Workdir != "/workspace/acme" {
			t.Fatalf("metadata = %+v", meta)
		}
		if meta.Environment["GOPATH"] != "/root/go" {
			t.Fatalf("environment = %v", meta.Environment)
		}
	})

	t.Run("partial metadata keeps defaults", func(t *testing.T) {
		write(`{"version":1,"workdir":"/workspace"}`)
		meta := ReadMetadata(lower)
		if meta.Shell != "/bin/sh" || meta.Hostname != "range" || meta.Workdir != "/workspace" {
			t.Fatalf("metadata = %+v", meta)
		}
	})

	t.Run("malformed metadata falls back to defaults", func(t *testing.T) {
		write(`{ not json`)
		meta := ReadMetadata(lower)
		if meta.Shell != "/bin/sh" || meta.Workdir != "/" {
			t.Fatalf("metadata = %+v", meta)
		}
	})
}
