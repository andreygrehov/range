package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/core"
)

// flagSet builds a command flag set carrying the common tuning flags.
func flagSet(name string, c *core.Config, trace *string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.StringVar(trace, "trace", "", "write an access trace (JSONL) to this path")
	fs.Func("block-size", "block size, e.g. 1MiB", func(v string) error {
		n, err := bytesize.Parse(v)
		if err != nil {
			return err
		}
		if n < 4096 {
			return errors.New("block size must be at least 4KiB")
		}
		c.BlockSize = n
		return nil
	})
	fs.Func("cache-size", "disk cache limit, e.g. 10GiB", func(v string) error {
		n, err := bytesize.Parse(v)
		c.DiskCache = n
		return err
	})
	fs.Func("memory-cache", "memory cache limit, e.g. 64MiB", func(v string) error {
		n, err := bytesize.Parse(v)
		c.MemoryCache = n
		return err
	})
	fs.Func("max-range", "largest coalesced remote request, e.g. 8MiB", func(v string) error {
		n, err := bytesize.Parse(v)
		c.MaxRangeSize = n
		return err
	})
	fs.Func("request-timeout", "ceiling on one remote request, e.g. 30s", func(v string) error {
		d, err := time.ParseDuration(v)
		if err != nil {
			return err
		}
		if d <= 0 {
			return errors.New("request timeout must be positive")
		}
		c.RequestTimeout = d
		return nil
	})
	fs.Func("cache-dir", "cache directory", func(v string) error {
		c.CacheDir = v
		return nil
	})
	fs.Func("prefetch", "on or off", func(v string) error {
		on, err := core.ParsePrefetch(v)
		c.Prefetch = on
		return err
	})
	return fs
}

// splitArgs separates flags from positional arguments so that flags may appear
// after the artifact, which is how the commands read most naturally:
//
//	range build ./rootfs --output dev.range
//	range shell s3://bucket/dev.range --workload go-test -- go test ./...
//
// Everything after a bare "--" is positional, which is how a workload command
// keeps its own flags.
func splitArgs(fs *flag.FlagSet, args []string) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i+1:]...)
			return flags, positional
		}
		if len(arg) > 1 && strings.HasPrefix(arg, "-") {
			flags = append(flags, arg)
			name := strings.TrimLeft(arg, "-")
			if strings.Contains(name, "=") {
				continue
			}
			// A non-boolean flag consumes the next argument as its value.
			if f := fs.Lookup(name); f != nil && !isBoolFlag(f) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, arg)
	}
	return flags, positional
}

func isBoolFlag(f *flag.Flag) bool {
	boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && boolean.IsBoolFlag()
}

// first non-flag argument.
func openFromArgs(name string, args []string, extra func(*flag.FlagSet)) (*core.Reader, core.Config, []string, error) {
	c, err := core.ReadConfig()
	if err != nil {
		return nil, c, nil, err
	}
	var tracePath string
	fs := flagSet(name, &c, &tracePath)
	if extra != nil {
		extra(fs)
	}
	flags, rest := splitArgs(fs, args)
	if err := fs.Parse(flags); err != nil {
		return nil, c, nil, err
	}
	if len(rest) == 0 {
		return nil, c, nil, fmt.Errorf("%s: missing <uri>", name)
	}
	if c.MaxRangeSize < c.BlockSize {
		c.MaxRangeSize = c.BlockSize
	}
	r, err := core.Open(context.Background(), rest[0], c)
	if err != nil {
		return nil, c, nil, err
	}
	if tracePath != "" {
		trace, err := core.NewTraceWriter(tracePath)
		if err != nil {
			r.Close()
			return nil, c, nil, err
		}
		r.Trace = trace
	}
	return r, c, rest[1:], nil
}
