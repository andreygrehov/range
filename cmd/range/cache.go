package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/cache"
	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/object"
)

func commandCache(args []string) error {
	if len(args) == 0 {
		return errors.New("cache: expected \"stats\" or \"clear\"")
	}
	c, err := core.ReadConfig()
	if err != nil {
		return err
	}
	root := filepath.Join(c.CacheDir, "objects")
	switch args[0] {
	case "stats":
		entries, err := os.ReadDir(root)
		if errors.Is(err, os.ErrNotExist) {
			fmt.Printf("Cache dir:      %s\nArtifacts:      0\nCached:         0 B\nLimit:          %s per artifact\n",
				c.CacheDir, bytesize.Format(c.DiskCache))
			return nil
		}
		if err != nil {
			return err
		}
		var total int64
		for _, entry := range entries {
			disk := &cache.Disk{Dir: filepath.Join(root, entry.Name())}
			used, err := disk.Scan()
			if err != nil {
				continue
			}
			total += used
		}
		fmt.Printf("Cache dir:      %s\n", c.CacheDir)
		fmt.Printf("Artifacts:      %d\n", len(entries))
		fmt.Printf("Cached:         %s\n", bytesize.Format(total))
		fmt.Printf("Limit:          %s per artifact\n", bytesize.Format(c.DiskCache))
		if layers := dirSize(filepath.Join(c.CacheDir, "oci")); layers > 0 {
			fmt.Printf("Image layers:   %s, indexes and layers read whole\n", bytesize.Format(layers))
		}
		return nil
	case "clear":
		if len(args) > 1 {
			return clearOneArtifact(root, args[1])
		}
		if err := os.RemoveAll(root); err != nil {
			return err
		}
		// Container image layers and their indexes are shared between images,
		// so they go only when the whole cache does.
		if err := os.RemoveAll(filepath.Join(c.CacheDir, "oci")); err != nil {
			return err
		}
		fmt.Println("Cache cleared.")
		return nil
	default:
		return fmt.Errorf("cache: unknown subcommand %q", args[0])
	}
}

func clearOneArtifact(root, uri string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	removed := 0
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		data, err := os.ReadFile(filepath.Join(path, "metadata.json"))
		if err != nil {
			continue
		}
		var ident object.Identity
		if err := json.Unmarshal(data, &ident); err != nil || ident.URI != uri {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		removed++
	}
	fmt.Printf("Cleared %d cached artifact(s) for %s.\n", removed, uri)
	return nil
}

// dirSize is the total size of the regular files under dir.
func dirSize(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
