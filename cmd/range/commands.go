package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andreygrehov/range/internal/commands"
	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/oci"
)

// commandCommands is the command map: "range commands which pdflatex" says
// which image a command runs from, and "range commands build", run by the
// catalog, makes the map from the images it indexes.
func commandCommands(args []string) error {
	if len(args) == 0 {
		return errors.New("commands: want range commands which COMMAND, or range commands build")
	}
	switch args[0] {
	case "which":
		return commandsWhich(args[1:])
	case "build":
		return commandsBuild(args[1:])
	}
	return fmt.Errorf("commands: no subcommand %q; want which or build", args[0])
}

// commandsClient looks commands up in the catalog's map, and keeps it in
// the cache for a day.
func commandsClient() (*commands.Client, error) {
	c, err := core.ReadConfig()
	if err != nil {
		return nil, err
	}
	base := oci.CatalogURL()
	if base == "" {
		return nil, errors.New("commands: the catalog is off (RANGE_INDEX_URL)")
	}
	return &commands.Client{
		Base: base, Dir: filepath.Join(c.CacheDir, "commands"), MaxAge: 24 * time.Hour,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}, nil
}

func commandsWhich(args []string) error {
	if len(args) != 1 {
		return errors.New("commands which: want range commands which COMMAND")
	}
	client, err := commandsClient()
	if err != nil {
		return err
	}
	owners, err := client.Lookup(context.Background(), args[0])
	if err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	for _, o := range owners {
		fmt.Printf("%s\t%s\n", o.Repo, strings.Join(o.Platforms, ", "))
	}
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
// what a repository brings, and "!COMMAND" for a command to leave out, with
// # comments.
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
		case len(fields) == 2:
			pins.Repo[fields[0]] = fields[1]
		default:
			return pins, fmt.Errorf("%s:%d: want COMMAND REPOSITORY, * REPOSITORY or !COMMAND", path, n)
		}
	}
	return pins, s.Err()
}
