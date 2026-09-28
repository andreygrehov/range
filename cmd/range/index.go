package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/andreygrehov/range/internal/oci"
)

// commandIndex builds the layer indexes of container images for a catalog,
// without running anything and without root. A first run of an image that a
// catalog already covers reads only what it touches.
func commandIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ContinueOnError)
	platforms := fs.String("platform", "linux/amd64,linux/arm64", "platforms to index, comma separated")
	out := fs.String("o", "index", "directory to write indexes into, as <ab>/sha256-<ab...>.idx")
	skip := fs.Bool("skip-published", true, "skip layers the catalog ($RANGE_INDEX_URL) already holds")
	flags, images := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(images) == 0 {
		return errors.New("index: want range index IMAGE... [--platform linux/amd64,linux/arm64] [-o DIR]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var failed []string
	for _, image := range images {
		name, ok := oci.ImageName(image)
		if !ok {
			name = image
		}
		for _, p := range strings.Split(*platforms, ",") {
			platform, err := oci.ParsePlatform(strings.TrimSpace(p))
			if err != nil {
				return err
			}
			if err := oci.IndexImage(ctx, name, platform, *out, *skip, func(line string) { fmt.Println(line) }); err != nil {
				fmt.Fprintf(os.Stderr, "range: %v\n", err)
				failed = append(failed, name+" "+platform.String())
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("index: %d image(s) failed: %s", len(failed), strings.Join(failed, ", "))
	}
	return nil
}
