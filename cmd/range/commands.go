package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andreygrehov/range/internal/commands"
	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/oci"
)

// commandCommands is the command map, for a catalog: "range commands build"
// makes the map from the images the catalog indexes, and "range commands
// sign" signs it.
func commandCommands(args []string) error {
	if len(args) == 0 {
		return errors.New("commands: want range commands build, or range commands sign")
	}
	switch args[0] {
	case "build":
		return commandsBuild(args[1:])
	case "sign":
		return commandsSign(args[1:])
	}
	return fmt.Errorf("commands: no subcommand %q; want build or sign", args[0])
}

// commandsDir is where Range keeps what it knows of command maps.
func commandsDir() (string, error) {
	c, err := core.ReadConfig()
	if err != nil {
		return "", err
	}
	return filepath.Join(c.CacheDir, "commands"), nil
}

// commandsClient looks commands up in the catalog's map, and keeps it in
// the cache for a day. Each catalog's map is kept apart: a root names its
// own catalog's shards, so a mirror must not answer with another's.
func commandsClient() (*commands.Client, error) {
	dir, err := commandsDir()
	if err != nil {
		return nil, err
	}
	base := oci.CatalogURL()
	if base == "" {
		return nil, errors.New("commands: the catalog is off (RANGE_INDEX_URL)")
	}
	keys, err := mapKeys()
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(base))
	return &commands.Client{
		Base: base, Dir: filepath.Join(dir, hex.EncodeToString(sum[:6])),
		// A root is small: asking for it each hour brings a new map within
		// the hour of its build.
		MaxAge: time.Hour, MaxStale: 30 * 24 * time.Hour, Keys: keys,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// mapKeys are the keys a catalog's map must be signed with: the public
// catalog's, and those in RANGE_MAP_KEYS, base64 and comma separated, for a
// catalog of one's own. RANGE_MAP_KEYS=off reads a map unsigned.
func mapKeys() (map[string]ed25519.PublicKey, error) {
	env := os.Getenv("RANGE_MAP_KEYS")
	if env == "off" {
		return nil, nil
	}
	keys := map[string]ed25519.PublicKey{}
	for _, encoded := range commands.CatalogKeys {
		key, err := commands.ParsePublicKey(encoded)
		if err != nil {
			return nil, err
		}
		keys[commands.KeyID(key)] = key
	}
	for _, encoded := range strings.Split(env, ",") {
		if strings.TrimSpace(encoded) == "" {
			continue
		}
		key, err := commands.ParsePublicKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("RANGE_MAP_KEYS: %w", err)
		}
		keys[commands.KeyID(key)] = key
	}
	return keys, nil
}

// commandsSign signs a map's root, as the catalog does after it builds one,
// with the base64 Ed25519 seed in an environment variable.
func commandsSign(args []string) error {
	fs := flag.NewFlagSet("commands sign", flag.ContinueOnError)
	keyEnv := fs.String("key-env", "MAP_SIGNING_KEY", "environment variable that holds the base64 Ed25519 seed")
	flags, files := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(files) != 1 {
		return errors.New("commands sign: want range commands sign [--key-env NAME] ROOT")
	}
	key, err := commands.ParsePrivateKey(os.Getenv(*keyEnv))
	if err != nil {
		return fmt.Errorf("%s: %w", *keyEnv, err)
	}
	root, err := os.ReadFile(files[0])
	if err != nil {
		return err
	}
	signed, err := commands.Sign(root, key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(files[0], signed, 0o644); err != nil {
		return err
	}
	fmt.Printf("signed %s with key %s\n", files[0], commands.KeyID(key.Public().(ed25519.PublicKey)))
	return nil
}

func commandsBuild(args []string) error {
	fs := flag.NewFlagSet("commands build", flag.ContinueOnError)
	list := fs.String("list", "", "file of images, one per line, as the catalog's lists")
	platforms := fs.String("platform", "linux/amd64,linux/arm64", "platforms to read, comma separated")
	pullsFile := fs.String("pulls", "", "file of REPOSITORY<TAB>PULLS, from Docker Hub")
	pinsFile := fs.String("pins", "", "file of COMMAND REPOSITORY lines, * REPOSITORY for a home, !COMMAND to leave out")
	digits := fs.Int("digits", 2, "hex digits that pick a command's shard")
	out := fs.String("o", "commands", "directory to write the map into, as the catalog serves it")
	cache := fs.String("cache", "", "directory to keep layer indexes in (default: a temporary one)")
	flags, images := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if *list != "" {
		listed, err := readImageList(*list)
		if err != nil {
			return err
		}
		images = append(images, listed...)
	}
	if len(images) == 0 {
		return errors.New("commands build: no images; give them as arguments or with --list")
	}
	pulls, err := readPulls(*pullsFile)
	if err != nil {
		return err
	}
	pins, err := readPins(*pinsFile)
	if err != nil {
		return err
	}
	if *cache == "" {
		dir, err := os.MkdirTemp("", "range-commands-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		*cache = dir
	}
	var plats []oci.Platform
	for _, p := range strings.Split(*platforms, ",") {
		platform, err := oci.ParsePlatform(strings.TrimSpace(p))
		if err != nil {
			return err
		}
		plats = append(plats, platform)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	type job struct {
		image    string
		platform oci.Platform
	}
	jobs := make(chan job)
	var (
		mu                 sync.Mutex
		found              []oci.ImageCommands
		notIndexed, failed int
	)
	var wg sync.WaitGroup
	for w := 0; w < 16; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				c, err := commandsOf(ctx, j.image, j.platform, *cache)
				mu.Lock()
				switch {
				case err == nil:
					found = append(found, c)
				case errors.Is(err, oci.ErrNotIndexed):
					notIndexed++
				case errors.As(err, new(*oci.PlatformError)):
					// The image is not built for this platform.
				default:
					failed++
					fmt.Fprintf(os.Stderr, "range: %s %s: %v\n", j.image, j.platform, err)
				}
				mu.Unlock()
			}
		}()
	}
	for _, image := range images {
		for _, p := range plats {
			select {
			case jobs <- job{image, p}:
			case <-ctx.Done():
			}
		}
	}
	close(jobs)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	m := commands.Build(found, pulls, pins)
	root, err := commands.Write(*out, m, *digits, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("%d images read, %d not in the catalog yet, %d failed\n", len(found), notIndexed, failed)
	fmt.Printf("%d commands in %d shards, in %s\n", len(m), len(root.Shards), *out)
	for command, repo := range pins.Repo {
		if owners := m[command]; len(owners) == 0 || owners[0].Repo != repo {
			fmt.Fprintf(os.Stderr, "range: pin %s %s: no image of %s holds %s\n", command, repo, repo, command)
		}
	}
	return nil
}

// commandsOf reads an image's commands, and tries again after a pause when a
// registry turns it away for now, as Docker Hub does past its rate limit. An
// image left out today would move commands between images from day to day.
func commandsOf(ctx context.Context, image string, platform oci.Platform, cache string) (oci.ImageCommands, error) {
	for attempt := 0; ; attempt++ {
		jctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		c, err := oci.Commands(jctx, image, platform, cache)
		cancel()
		final := err == nil || errors.Is(err, oci.ErrNotIndexed) || errors.As(err, new(*oci.PlatformError))
		if final || attempt == 3 || ctx.Err() != nil {
			return c, err
		}
		select {
		case <-time.After(time.Duration(30<<attempt) * time.Second):
		case <-ctx.Done():
		}
	}
}

// readImageList reads a list of images as the catalog keeps them: one per
// line, "IMAGE [| COMMAND]", with # comments.
func readImageList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var images []string
	s := bufio.NewScanner(f)
	for s.Scan() {
		line, _, _ := strings.Cut(s.Text(), "#")
		image, _, _ := strings.Cut(line, "|")
		if image = strings.TrimSpace(image); image != "" {
			images = append(images, image)
		}
	}
	return images, s.Err()
}

// readPulls reads REPOSITORY<TAB>PULLS lines. A line may name an image with
// its tag, as the catalog's list of the most pulled images does.
func readPulls(path string) (map[string]int64, error) {
	pulls := map[string]int64{}
	if path == "" {
		return pulls, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) != 2 {
			continue
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		repo := commands.Repository(fields[0])
		pulls[repo] = max(pulls[repo], n)
	}
	return pulls, s.Err()
}

// readPins reads "COMMAND REPOSITORY" lines, "* REPOSITORY" for the home of
// what a repository brings, "+ REPOSITORY" or "+ NAMESPACE/*" for one a
// session trusts, and "!COMMAND" for a command to leave out, with # comments.
func readPins(path string) (commands.Pins, error) {
	pins := commands.Pins{Repo: map[string]string{}, Home: map[string]bool{}, Skip: map[string]bool{}}
	if path == "" {
		return pins, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return pins, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for n := 1; s.Scan(); n++ {
		line, _, _ := strings.Cut(s.Text(), "#")
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case len(fields) == 1 && strings.HasPrefix(fields[0], "!"):
			pins.Skip[fields[0][1:]] = true
		case len(fields) == 2 && fields[0] == "*":
			pins.Home[fields[1]] = true
		case len(fields) == 2 && fields[0] == "+":
			pins.Trust = append(pins.Trust, fields[1])
		case len(fields) == 2:
			pins.Repo[fields[0]] = fields[1]
		default:
			return pins, fmt.Errorf("%s:%d: want COMMAND REPOSITORY, * REPOSITORY, + REPOSITORY or !COMMAND", path, n)
		}
	}
	return pins, s.Err()
}

// catalogFinder finds, for a session, the image of a command it lacks: the
// first one the catalog's map trusts that is built for this platform.
type catalogFinder struct {
	client *commands.Client
}

func newCatalogFinder() (catalogFinder, error) {
	client, err := commandsClient()
	return catalogFinder{client}, err
}

// platform is what a session runs: Linux on this machine's architecture.
func (catalogFinder) platform() string { return "linux/" + runtime.GOARCH }

func (f catalogFinder) usable(o commands.Owner) bool {
	// A map from before trust was marked trusts official images, whose
	// names have no namespace.
	trusted := o.Trusted || !strings.Contains(o.Repo, "/")
	return trusted && slices.Contains(o.Platforms, f.platform())
}

func (f catalogFinder) Names(ctx context.Context) ([]string, error) {
	return f.client.Names(ctx, func(owners []commands.Owner) bool {
		return slices.ContainsFunc(owners, f.usable)
	})
}

func (f catalogFinder) Find(ctx context.Context, command, version string) (image, program string, err error) {
	owners, err := f.client.Lookup(ctx, command)
	if err != nil {
		return "", "", err
	}
	return pickOwner(owners, f.usable, command, version)
}

// pickOwner takes the first owner usable says yes to. Its tag is the
// version asked for, or the one the catalog read.
func pickOwner(owners []commands.Owner, usable func(commands.Owner) bool, command, version string) (image, program string, err error) {
	for _, o := range owners {
		if !usable(o) {
			continue
		}
		tag := version
		if tag == "" {
			tag = o.Tag
		}
		if tag == "" {
			tag = "latest"
		}
		program = command
		if o.Path != "" && version == "" {
			program = o.Path
		}
		return o.Repo + ":" + tag, program, nil
	}
	if len(owners) > 0 {
		return "", "", fmt.Errorf("%s is in %s, which this catalog does not trust for a session, or not built for this platform", command, owners[0].Repo)
	}
	return "", "", commands.ErrUnknown
}
