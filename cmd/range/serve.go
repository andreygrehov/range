package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
	"github.com/andreygrehov/range/internal/core"
	"github.com/andreygrehov/range/internal/profile"
)

// commandServe exposes an artifact as an ordinary HTTP resource with Range
// support on localhost. Anything that reads a URL by range - DuckDB, SQLite's
// HTTP VFS, GDAL's /vsicurl/, ffmpeg, curl - then gets Range's disk cache,
// request coalescing and learned working set without knowing Range exists.
// For a Range artifact the bytes served are the decompressed, hash-verified
// logical ones.
func commandServe(args []string) error {
	var addr, workload, allowHosts string
	r, c, _, err := openFromArgs("serve", args, func(fs *flag.FlagSet) {
		fs.StringVar(&addr, "addr", "127.0.0.1:8003", "listen address")
		fs.StringVar(&allowHosts, "allow-host", "", "comma-separated host names clients may use, besides localhost and IP addresses")
		fs.StringVar(&workload, "workload", "serve", "profile name for what this endpoint's readers do")
	})
	if err != nil {
		return err
	}
	defer r.Close()
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.ProfileMode == core.ProfileAuto {
		if learned, ok := profile.Load(c.CacheDir, r.Ident, r.BlockSize, workload); ok {
			go r.PrefetchProfile(ctx, learned, c.PrefetchLimit)
		}
	}
	r.Workload = workload
	go r.PublishStats(5 * time.Second)

	name := servedName(r.Ident.URI)
	var allowed []string
	for _, h := range strings.Split(allowHosts, ",") {
		if h = strings.TrimSpace(h); h != "" {
			allowed = append(allowed, h)
		}
	}
	server := &http.Server{Handler: serveHandler(r, name, allowed), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	fmt.Fprintf(os.Stderr, "Serving %s (%s)\n  http://%s/%s\n\nRange requests are served from the local cache; Ctrl-C to stop.\n",
		r.Ident.URI, bytesize.Format(r.Size()), listener.Addr(), name)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if c.ProfileMode != core.ProfileOff && r.Recorder.Count() > 0 {
		if _, err := profile.Save(c.CacheDir, r.Ident, r.BlockSize, workload,
			r.Recorder.Observations(), time.Now()); err != nil {
			log.Printf("range: could not save working-set profile: %v", err)
		}
	}
	printStats(r.Snapshot())
	return nil
}

// serveHandler answers GET and HEAD for one artifact, with byte ranges,
// multi-range and conditional requests handled by net/http. The ETag is the
// artifact identity, so a client's cache is invalidated exactly when the
// underlying object changes.
//
// The endpoint reads with the caller's cloud credentials, so a web page must
// not be able to reach it by rebinding its own domain to 127.0.0.1. That needs
// a host name, so requests are answered only for IP addresses, localhost, and
// the names in allowed.
func serveHandler(r *core.Reader, name string, allowed []string) http.Handler {
	etag := `"` + r.Ident.Key() + `"`
	mux := http.NewServeMux()
	mux.HandleFunc("/"+name, func(w http.ResponseWriter, req *http.Request) {
		if !serveHostAllowed(req.Host, allowed) {
			http.Error(w, "host not allowed; pass --allow-host to serve under this name", http.StatusForbidden)
			return
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, req, name, r.Ident.LastModified, io.NewSectionReader(r, 0, r.Size()))
	})
	return mux
}

func serveHostAllowed(host string, allowed []string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return false
	}
	if net.ParseIP(host) != nil || strings.EqualFold(host, "localhost") {
		return true
	}
	for _, name := range allowed {
		if strings.EqualFold(host, name) {
			return true
		}
	}
	return false
}

// servedName is the last path element of the source, which is what a client
// will expect to see at the end of the URL.
func servedName(uri string) string {
	if parsed, err := url.Parse(uri); err == nil && parsed.Path != "" && parsed.Scheme != "" {
		if base := path.Base(parsed.Path); base != "/" && base != "." {
			return base
		}
	}
	if base := filepath.Base(uri); base != "/" && base != "." {
		return base
	}
	return "artifact"
}
