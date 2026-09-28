package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/object"
	"github.com/andreygrehov/range/internal/profile"
)

// Profiles are ordinary files. They can be exported, copied to another machine
// and imported, which is what makes cross-machine working-set experiments
// possible without any server.
func commandProfile(args []string) error {
	if len(args) == 0 {
		return errors.New("profile: expected path, show, export, import or clear")
	}
	c, err := core.ReadConfig()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("profile", flag.ContinueOnError)
	workload := fs.String("workload", profile.DefaultWorkload, "workload name")
	output := fs.String("output", "", "file to write (export)")
	flags, rest := splitArgs(fs, args[1:])
	if err := fs.Parse(flags); err != nil {
		return err
	}

	needArtifact := func() (object.Identity, error) {
		if len(rest) == 0 {
			return object.Identity{}, errors.New("profile: missing <uri>")
		}
		r, err := core.Open(context.Background(), rest[0], c)
		if err != nil {
			return object.Identity{}, err
		}
		defer r.Close()
		return r.Ident, nil
	}

	switch args[0] {
	case "path":
		ident, err := needArtifact()
		if err != nil {
			return err
		}
		fmt.Println(profile.Path(c.CacheDir, ident, *workload))
		return nil

	case "show":
		ident, err := needArtifact()
		if err != nil {
			return err
		}
		p, err := profile.ReadFile(profile.Path(c.CacheDir, ident, *workload))
		if err != nil {
			return fmt.Errorf("no profile for workload %q: %w", *workload, err)
		}
		printProfile(p)
		return nil

	case "export":
		ident, err := needArtifact()
		if err != nil {
			return err
		}
		source := profile.Path(c.CacheDir, ident, *workload)
		data, err := os.ReadFile(source)
		if err != nil {
			return fmt.Errorf("no profile for workload %q: %w", *workload, err)
		}
		if *output == "" || *output == "-" {
			_, err := os.Stdout.Write(data)
			return err
		}
		if err := os.WriteFile(*output, data, 0o644); err != nil {
			return err
		}
		fmt.Printf("Exported %s\n", *output)
		return nil

	case "import":
		if len(rest) == 0 {
			return errors.New("profile import: missing <file>")
		}
		p, err := profile.ReadFile(rest[0])
		if err != nil {
			return err
		}
		if p.Workload == "" || p.BlockSize == 0 {
			return errors.New("profile import: file is missing workload or block size")
		}
		ident := object.Identity{
			URI: p.Artifact.URI, Size: p.Artifact.Size, ETag: p.Artifact.ETag,
			VersionID: p.Artifact.VersionID, BlockSize: p.BlockSize,
		}
		target := profile.Path(c.CacheDir, ident, p.Workload)
		if err := profile.WriteFile(target, p); err != nil {
			return err
		}
		fmt.Printf("Imported %s (%d sessions, %d ranges) for workload %q\n",
			p.Artifact.URI, p.Sessions, len(p.Ranges), p.Workload)
		return nil

	case "clear":
		root := filepath.Join(c.CacheDir, "profiles")
		if len(rest) == 0 {
			if err := os.RemoveAll(root); err != nil {
				return err
			}
			fmt.Println("Learned profiles cleared.")
			return nil
		}
		ident, err := needArtifact()
		if err != nil {
			return err
		}
		removed := 0
		if err := os.Remove(profile.Path(c.CacheDir, ident, *workload)); err == nil {
			removed++
		}
		fmt.Printf("Cleared %d profile(s) for %s workload %q.\n", removed, ident.URI, *workload)
		return nil

	default:
		return fmt.Errorf("profile: unknown subcommand %q", args[0])
	}
}

func printProfile(p profile.Profile) {
	fmt.Printf("Artifact            %s\n", p.Artifact.URI)
	fmt.Printf("  Size              %s\n", bytesize.Format(p.Artifact.Size))
	fmt.Printf("  ETag              %s\n", orDash(p.Artifact.ETag))
	fmt.Printf("  Version           %s\n", orDash(p.Artifact.VersionID))
	fmt.Printf("Profile\n")
	fmt.Printf("  Workload          %s\n", p.Workload)
	fmt.Printf("  Block size        %s\n", bytesize.Format(p.BlockSize))
	fmt.Printf("  Sessions          %d\n", p.Sessions)
	fmt.Printf("  Ranges            %d\n", len(p.Ranges))
	fmt.Printf("  Blocks            %d\n", p.BlockTotal())
	fmt.Printf("  Working set       %s\n", bytesize.Format(p.BlockTotal()*p.BlockSize))
	fmt.Printf("  Updated           %s\n", orDash(p.UpdatedAt))
}

// commandReset clears both halves of local state: materialized bytes and
// learned profiles. Benchmarks need each separately, so reset is explicit.
func commandReset(args []string) error {
	c, err := core.ReadConfig()
	if err != nil {
		return err
	}
	if err := commandCache(append([]string{"clear"}, args...)); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(c.CacheDir, "profiles")); err != nil {
		return err
	}
	fmt.Println("Learned profiles cleared.")
	return nil
}
