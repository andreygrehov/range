package commands

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Client looks commands up in a catalog's map, and keeps what it fetched in
// a directory. A shard, named by its content, is fetched once. The root is
// fetched again when it is older than MaxAge, and a client that cannot reach
// the catalog keeps using the root it has.
//
// With Keys, a root must carry the signature of one of them. A fresh root
// built before the one kept is refused, so a mirror cannot hand back an old
// map, and a root built longer than MaxStale ago is not used.
type Client struct {
	Base     string // the catalog: https://github.com/andreygrehov/range-index/releases/download
	Dir      string // where the root and shards are kept
	MaxAge   time.Duration
	MaxStale time.Duration // 0: no limit
	Keys     map[string]ed25519.PublicKey
	HTTP     *http.Client
	Now      func() time.Time
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

// Names returns the commands in the map whose owners keep says yes to,
// sorted: what shims are made for. It fetches the shards it does not have, a
// few at a time.
func (c *Client) Names(ctx context.Context, keep func([]Owner) bool) ([]string, error) {
	root, err := c.root(ctx)
	if err != nil {
		return nil, err
	}
	var (
		mu    sync.Mutex
		names []string
		first error
		wg    sync.WaitGroup
	)
	slots := make(chan struct{}, 16)
	for _, digest := range root.Shards {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			shard, err := c.shard(ctx, digest)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if first == nil {
					first = err
				}
				return
			}
			for name, owners := range shard {
				if keep == nil || keep(owners) {
					names = append(names, name)
				}
			}
		}()
	}
	wg.Wait()
	if first != nil {
		return nil, first
	}
	sort.Strings(names)
	return names, nil
}

func (c *Client) root(ctx context.Context) (Root, error) {
	path := filepath.Join(c.Dir, "commands.json")
	kept, keptErr := c.readRoot(path)
	if keptErr == nil {
		if info, err := os.Stat(path); err == nil && c.now().Sub(info.ModTime()) < c.MaxAge {
			return kept, c.fresh(kept)
		}
	}
	data, err := c.get(ctx, RootName)
	if err != nil {
		err = fmt.Errorf("commands: the catalog's map: %w", err)
	} else {
		var fresh Root
		if fresh, err = c.parseRoot(data); err == nil {
			if keptErr == nil && fresh.Built.Before(kept.Built) {
				err = fmt.Errorf("commands: the catalog's map was built %s, before the one kept here", fresh.Built.Format(time.RFC3339))
			} else {
				writeFile(path, data) // kept only to save the next fetch
				return fresh, c.fresh(fresh)
			}
		}
	}
	if keptErr == nil {
		// The catalog is out of reach or broken: the map kept here still
		// answers, a day or a week old.
		return kept, c.fresh(kept)
	}
	return Root{}, err
}

func (c *Client) readRoot(path string) (Root, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Root{}, err
	}
	return c.parseRoot(data)
}

// parseRoot checks a root's signature, when the client has keys, then reads
// it.
func (c *Client) parseRoot(data []byte) (Root, error) {
	if len(c.Keys) > 0 {
		if _, err := Verify(data, c.Keys); err != nil {
			return Root{}, err
		}
	}
	var r Root
	if err := json.Unmarshal(data, &r); err != nil {
		return Root{}, err
	}
	return r, check(r)
}

// fresh refuses a root built longer than MaxStale ago: a catalog that stopped
// building, or someone who keeps an old one in place.
func (c *Client) fresh(r Root) error {
	if age := c.now().Sub(r.Built); c.MaxStale > 0 && age > c.MaxStale {
		return fmt.Errorf("commands: the catalog's map is %d days old; update it, or set RANGE_INDEX_URL", int(age.Hours()/24))
	}
	return nil
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
