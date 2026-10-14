// Package commands is the command map: which container image holds a command
// that this machine does not have, such as pdflatex or nvcc, so that Range
// can run it from there.
//
// The catalog builds the map from the commands of the images it indexes, and
// publishes it as a small root and shards named by their content. A client
// fetches the root, then the one shard its command falls in, and keeps both.
// Looking up a command costs the same however many images the catalog holds.
package commands

import (
	"regexp"
	"sort"
	"strings"

	"github.com/andreygrehov/range/internal/oci"
)

// Owner is an image repository that holds a command, and the platforms it
// holds it for.
type Owner struct {
	Repo      string   `json:"repo"`
	Platforms []string `json:"platforms"`
}

// Map lists, for each command, the repositories that hold it, best first.
type Map map[string][]Owner

// maxOwners bounds the repositories kept for a command. The first one that
// suits the platform runs it. The rest are for the platforms it lacks.
const maxOwners = 8

// validName is a command name the map keeps: a file name a shell can run by
// typing it. It rules out names such as "[" and ones with spaces.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Pins are decisions people make where the data cannot: which image a
// command runs from, and which commands the map leaves out.
type Pins struct {
	// Repo maps a command to the repository that runs it first.
	Repo map[string]string
	// Skip lists commands the map leaves out, such as daemons.
	Skip map[string]bool
}

// Build makes the map from the commands of images.
//
// A command belongs to the repository that brought it, not to every image
// that inherits it:
//
//  1. A command whose path the image's OS layer already has is the OS's.
//  2. A layer belongs to the repository of the smallest image that has it,
//     the image it was built for. An image built on another inherits its
//     layers, and their commands stay its parent's.
//  3. A repository named for a command ranks first: python for python3.12,
//     erlang for erl, and an image whose entrypoint is the command.
//  4. A command in the same layer as one of those ranks next: pip, which
//     comes with python3.
//  5. Popularity breaks ties within those. For any other command, the
//     smallest image that has it wins: it costs the least to fetch.
//
// Pins then put a chosen repository first and leave skipped commands out.
func Build(images []oci.ImageCommands, pulls map[string]int64, pins Pins) Map {
	type parentOf struct {
		layers   int
		repo     string
		platform string
	}
	parent := map[string]parentOf{}
	entrypoints := map[string]map[string]bool{}
	for _, img := range images {
		repo := Repository(img.Image)
		for _, d := range img.Layers {
			p, ok := parent[d]
			if !ok || len(img.Layers) < p.layers || (len(img.Layers) == p.layers && repo < p.repo) {
				parent[d] = parentOf{len(img.Layers), repo, img.Platform}
			}
		}
		if img.Entrypoint != "" {
			if entrypoints[img.Entrypoint] == nil {
				entrypoints[img.Entrypoint] = map[string]bool{}
			}
			entrypoints[img.Entrypoint][repo] = true
		}
	}
	named := func(command, repo string) bool { return namedFor(command, repo, entrypoints[command][repo]) }

	// A layer that brings a command named for its repository is that
	// repository's own.
	own := map[string]string{}
	for _, img := range images {
		repo, inOS := Repository(img.Image), set(img.OS)
		for command, layer := range img.Commands {
			if !inOS[command] && named(command, repo) {
				own[img.Layers[layer]] = repo
			}
		}
	}

	type candidate struct {
		rank      rank
		platforms map[string]bool
	}
	found := map[string]map[string]*candidate{}
	for _, img := range images {
		inOS := set(img.OS)
		for command, layer := range img.Commands {
			if inOS[command] || pins.Skip[command] || !validName.MatchString(command) {
				continue
			}
			d := img.Layers[layer]
			repo, tier := parent[d].repo, 2
			if o, ok := own[d]; ok {
				repo, tier = o, 1
			}
			if named(command, repo) {
				tier = 0
			}
			r := rankOf(tier, pulls[repo], parent[d].layers)
			if found[command] == nil {
				found[command] = map[string]*candidate{}
			}
			c := found[command][repo]
			if c == nil {
				c = &candidate{rank: r, platforms: map[string]bool{}}
				found[command][repo] = c
			}
			if r.better(c.rank) {
				c.rank = r
			}
			c.platforms[parent[d].platform] = true
		}
	}

	m := Map{}
	for command, repos := range found {
		names := make([]string, 0, len(repos))
		for repo := range repos {
			names = append(names, repo)
		}
		pinned := pins.Repo[command]
		sort.Slice(names, func(i, j int) bool {
			a, b := names[i], names[j]
			if (a == pinned) != (b == pinned) {
				return a == pinned
			}
			if ra, rb := repos[a].rank, repos[b].rank; ra != rb {
				return ra.better(rb)
			}
			return a < b
		})
		if len(names) > maxOwners {
			names = names[:maxOwners]
		}
		for _, repo := range names {
			m[command] = append(m[command], Owner{Repo: repo, Platforms: keys(repos[repo].platforms)})
		}
	}
	return m
}

// namedFor reports whether a repository is named for a command: an exact
// name, a name and a version (python for python3.12), a shortened name
// (erlang for erl), or an image whose entrypoint is the command.
func namedFor(command, repo string, entrypoint bool) bool {
	name := repo[strings.LastIndex(repo, "/")+1:]
	if command == name || entrypoint {
		return true
	}
	if rest, ok := strings.CutPrefix(command, name); ok && strings.Trim(rest, "0123456789.-") == "" {
		return true
	}
	return len(command) >= 2 && strings.HasPrefix(name, command)
}

// Repository is an image's name without its tag or digest:
// ghcr.io/org/app for ghcr.io/org/app:1.2.
func Repository(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	if colon := strings.LastIndex(image, ":"); colon > strings.LastIndex(image, "/") {
		image = image[:colon]
	}
	return image
}

// rank orders the repositories that hold a command. Lower is better.
type rank struct {
	tier int // 0: named for the command, 1: in its own layer, 2: any other
	// For tiers 0 and 1, the more pulls the better, then nothing. For tier
	// 2, the fewer layers the better, then the more pulls.
	first, then int64
}

func rankOf(tier int, pulls int64, layers int) rank {
	if tier < 2 {
		return rank{tier, -pulls, 0}
	}
	return rank{tier, int64(layers), -pulls}
}

func (a rank) better(b rank) bool {
	if a.tier != b.tier {
		return a.tier < b.tier
	}
	if a.first != b.first {
		return a.first < b.first
	}
	return a.then < b.then
}

func set(names []string) map[string]bool {
	s := make(map[string]bool, len(names))
	for _, n := range names {
		s[n] = true
	}
	return s
}

func keys(s map[string]bool) []string {
	out := make([]string, 0, len(s))
	for k := range s {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
