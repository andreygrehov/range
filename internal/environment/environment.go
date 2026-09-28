package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Metadata is optional. An image with no /etc/range/environment.json
// still starts, using these defaults.
type Metadata struct {
	Version     int               `json:"version"`
	Name        string            `json:"name"`
	Shell       string            `json:"shell"`
	Workdir     string            `json:"workdir"`
	Hostname    string            `json:"hostname"`
	Environment map[string]string `json:"environment"`
	// Platform is what the artifact's binaries were built for, linux/ARCH.
	// Empty for a directory build, whose contents Range did not choose.
	Platform string `json:"platform,omitempty"`
	// Entrypoint and Cmd are a container image's own command, which
	// "range run <uri>" with no command after -- runs, as docker run does.
	Entrypoint []string `json:"entrypoint,omitempty"`
	Cmd        []string `json:"cmd,omitempty"`
}

// Command is what the environment runs when it is given no command.
func (m Metadata) Command() []string {
	return append(append([]string(nil), m.Entrypoint...), m.Cmd...)
}

// ReadMetadata reads the metadata of the filesystem mounted at lower, falling
// back to defaults for anything it does not say.
func ReadMetadata(lower string) Metadata {
	meta := Metadata{Shell: "/bin/sh", Workdir: "/", Hostname: "range"}
	data, err := os.ReadFile(filepath.Join(lower, "etc", "range", "environment.json"))
	if err != nil {
		return meta
	}
	var parsed Metadata
	if err := json.Unmarshal(data, &parsed); err != nil {
		return meta
	}
	if parsed.Name != "" {
		meta.Name = parsed.Name
	}
	if parsed.Shell != "" {
		meta.Shell = parsed.Shell
	}
	if parsed.Workdir != "" {
		meta.Workdir = parsed.Workdir
	}
	if parsed.Hostname != "" {
		meta.Hostname = parsed.Hostname
	}
	meta.Environment = parsed.Environment
	meta.Platform, meta.Entrypoint, meta.Cmd = parsed.Platform, parsed.Entrypoint, parsed.Cmd
	return meta
}
