package oci

import (
	"fmt"
	"sort"
	"testing"
)

func file(name string, mode int64) Entry { return Entry{Name: name, Typeflag: '0', Mode: mode} }
func link(name, target string) Entry     { return Entry{Name: name, Typeflag: '2', Linkname: target} }

func TestCommandsOf(t *testing.T) {
	layers := [][]Entry{
		// The OS, with a merged /usr.
		{file("etc/os-release", 0o644), link("bin", "usr/bin"), file("usr/bin/ls", 0o755),
			file("usr/bin/sh", 0o755)},
		// What the image adds: Python, CUDA behind a linked directory, an
		// updated ls, and a file that may not run.
		{file("usr/local/bin/python3.12", 0o755), link("usr/local/bin/python3", "python3.12"),
			file("usr/local/bin/pip", 0o755), file("usr/local/bin/README", 0o644),
			link("usr/local/cuda", "cuda-12.8"), file("usr/local/cuda-12.8/bin/nvcc", 0o755),
			file("usr/bin/ls", 0o755), file("jq", 0o755)},
		// A later layer removes pip.
		{file("usr/local/bin/.wh.pip", 0)},
	}
	var config ImageConfig
	config.Config.Env = []string{"PATH=/usr/local/cuda/bin:/usr/local/bin:/usr/bin:/bin"}
	config.Config.Entrypoint = []string{"/jq"}
	c := commandsOf(layers, config)

	want := map[string]int{"python3.12": 1, "python3": 1, "nvcc": 1, "ls": 1, "sh": 0, "jq": 1}
	if fmt.Sprint(c.Commands) != fmt.Sprint(want) {
		t.Errorf("commands = %v, want %v", c.Commands, want)
	}
	sort.Strings(c.OS)
	if fmt.Sprint(c.OS) != "[ls sh]" {
		t.Errorf("OS = %v: an updated ls is still the OS's", c.OS)
	}
	if c.Entrypoint != "jq" {
		t.Errorf("entrypoint = %q", c.Entrypoint)
	}
}

func TestCommandsOfAnImageFromScratch(t *testing.T) {
	var config ImageConfig
	config.Config.Entrypoint = []string{"/jq"}
	c := commandsOf([][]Entry{{file("jq", 0o755)}, {file("LICENSE", 0o644)}}, config)
	if fmt.Sprint(c.Commands) != "map[jq:0]" || len(c.OS) != 0 {
		t.Errorf("commands = %v, OS = %v", c.Commands, c.OS)
	}
}

func TestAnOpaqueDirectoryHidesWhatIsBelow(t *testing.T) {
	layers := [][]Entry{
		{file("opt/tool/bin/old", 0o755), file("opt/tool/bin/kept", 0o755)},
		{file("opt/tool/.wh..wh..opq", 0), file("opt/tool/bin/new", 0o755)},
	}
	var config ImageConfig
	config.Config.Env = []string{"PATH=/opt/tool/bin"}
	if c := commandsOf(layers, config); fmt.Sprint(c.Commands) != "map[new:1]" {
		t.Errorf("commands = %v", c.Commands)
	}
}
