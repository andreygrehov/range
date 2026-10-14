package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// Client looks commands up in a catalog's map, and keeps what it fetched in
// a directory. A shard, named by its content, is fetched once. The root is
// fetched again when it is older than MaxAge, and a client that cannot reach
// the catalog keeps using the root it has.
type Client struct {
	Base   string // the catalog: https://github.com/andreygrehov/range-index/releases/download
	Dir    string // where the root and shards are kept
	MaxAge time.Duration
	HTTP   *http.Client
	Now    func() time.Time
}

// ErrUnknown says that no image in the map holds a command.
var ErrUnknown = errors.New("no image in the catalog holds this command")

// Lookup returns the repositories that hold command, best first.
func (c *Client) Lookup(ctx context.Context, command string) ([]Owner, error) {
	root, err := c.root(ctx)
	if err != nil {
		return nil, err
	}
	digest, ok := root.Shards[prefixOf(command, root.Digits)]
	if !ok {
		return nil, ErrUnknown
	}
	shard, err := c.shard(ctx, digest)
	if err != nil {
		return nil, err
	}
	owners, ok := shard[command]
	if !ok {
		return nil, ErrUnknown
	}
	return owners, nil
}

func (c *Client) root(ctx context.Context) (Root, error) {
	path := filepath.Join(c.Dir, "commands.json")
	kept, keptErr := readRoot(path)
	if keptErr == nil {
		if info, err := os.Stat(path); err == nil && c.now().Sub(info.ModTime()) < c.MaxAge {
			return kept, nil
		}
	}
	data, err := c.get(ctx, RootName)
	if err == nil {
		var fresh Root
		if err = json.Unmarshal(data, &fresh); err == nil {
			if err = check(fresh); err == nil {
				writeFile(path, data) // kept only to save the next fetch
				return fresh, nil
			}
		}
	}
	if keptErr == nil {
		// The catalog is out of reach or broken: the map kept here still
		// answers, a day or a week old.
		return kept, nil
	}
	return Root{}, fmt.Errorf("commands: the catalog's map: %w", err)
}

func readRoot(path string) (Root, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Root{}, err
	}
	var r Root
	if err := json.Unmarshal(data, &r); err != nil {
		return Root{}, err
	}
	return r, check(r)
}

// check refuses a root this Range cannot read.
func check(r Root) error {
	switch {
	case r.Format > Format:
		return fmt.Errorf("commands: the catalog's map is format %d, and this Range reads %d: update Range", r.Format, Format)
	case r.Format < 1 || r.Digits < 1 || r.Digits > 8:
		return errors.New("commands: the catalog's map has no valid root")
	}
	return nil
}

func (c *Client) shard(ctx context.Context, digest string) (Map, error) {
	path := filepath.Join(c.Dir, "shards", filepath.Base(shardPath(digest)))
	if data, err := os.ReadFile(path); err == nil {
		if m, err := decodeShard(data, digest); err == nil {
			return m, nil
		}
	}
	data, err := c.get(ctx, shardPath(digest))
	if err != nil {
		return nil, err
	}
	m, err := decodeShard(data, digest)
	if err != nil {
		return nil, err
	}
	writeFile(path, data) // kept only to save the next fetch
	return m, nil
}

// get fetches a file of the catalog. The map's files are small, so a
// megabyte bounds a catalog that sends something else.
func (c *Client) get(ctx context.Context, name string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Base+"/"+name, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", name, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}
