package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/publish"
)

// commandPublish uploads a built environment to object storage. It is the only
// write path in Range: everything else treats a remote object as immutable.
//
// There is deliberately no "pull". Publishing is the one transfer of the whole
// artifact that ever happens; every consumer reads the part it needs in place.
func commandPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	partSize := fs.String("part-size", "64MiB", "size of each multipart part")
	parallel := fs.Int("parallel", 8, "concurrent part uploads")
	endpoint := fs.String("endpoint", os.Getenv("RANGE_S3_ENDPOINT"), "S3-compatible endpoint")
	flags, rest := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(rest) != 2 {
		return errors.New("publish: usage is range publish <file> s3://bucket/key")
	}
	source, target := rest[0], rest[1]
	if !strings.HasPrefix(target, "s3://") {
		return fmt.Errorf("publish: %s is not an s3:// uri", target)
	}
	bucket, key, err := object.SplitS3URI(target)
	if err != nil {
		return err
	}
	part, err := bytesize.Parse(*partSize)
	if err != nil {
		return err
	}
	if part < 5<<20 {
		return errors.New("publish: --part-size must be at least 5MiB")
	}
	if *parallel < 1 {
		return errors.New("publish: --parallel must be at least 1")
	}
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	backend, err := object.NewS3(*endpoint)
	if err != nil {
		return err
	}
	ctx := context.Background()
	started := time.Now()
	fmt.Fprintf(os.Stderr, "Publishing %s (%s) to %s\n", source, bytesize.Format(info.Size()), target)
	if err := publish.Upload(ctx, backend.Client, bucket, key, file, info.Size(), part, *parallel); err != nil {
		return err
	}
	elapsed := time.Since(started)
	fmt.Printf("\nPublished\n")
	fmt.Printf("  Artifact   %s\n", target)
	fmt.Printf("  Size       %s\n", bytesize.Format(info.Size()))
	fmt.Printf("  Uploaded   %s\n", elapsed.Round(time.Millisecond))
	fmt.Printf("\nEnter it with:\n  range shell %s\n", target)
	return nil
}
