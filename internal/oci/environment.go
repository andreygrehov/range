package oci

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/andreygrehov/range/internal/environment"
)

// WriteEnvironment carries the image's own PATH, environment and working
// directory into the artifact, so a shell opened on it behaves like the image.
func WriteEnvironment(dir, image string, config ImageConfig, platform Platform) error {
	meta := environmentFor(image, config, platform, func(p string) bool {
		_, err := os.Stat(filepath.Join(dir, p))
		return err == nil
	})
	if err := os.MkdirAll(filepath.Join(dir, "etc", "range"), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "etc", "range", "environment.json"), body, 0o644)
}

// environmentFor carries an image's PATH, environment and working directory
// into Range's metadata. exists answers whether a path exists in the image, so
// the shell can be chosen without assuming where the image lives.
func environmentFor(image string, config ImageConfig, platform Platform, exists func(string) bool) environment.Metadata {
	env := map[string]string{}
	for _, entry := range config.Config.Env {
		if key, value, found := strings.Cut(entry, "="); found {
			env[key] = value
		}
	}
	name := strings.NewReplacer("/", "-", ":", "-").Replace(image)
	meta := environment.Metadata{
		Version: 1, Name: name, Shell: "/bin/sh", Hostname: "range",
		Workdir: config.Config.WorkingDir, Environment: env,
		Platform: platform.os + "/" + platform.arch,
	}
	if meta.Workdir == "" {
		meta.Workdir = "/"
	}
	for _, shell := range []string{"/bin/bash", "/bin/sh"} {
		if exists(strings.TrimPrefix(shell, "/")) {
			meta.Shell = shell
			break
		}
	}
	return meta
}
