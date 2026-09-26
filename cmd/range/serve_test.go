package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/andreygrehov/range/internal/core/coretest"
	"github.com/andreygrehov/range/internal/rangetest"
)

// A page on evil.example that rebinds its name to 127.0.0.1 reaches the
// listener, but its requests carry Host: evil.example and must be refused
// before a single byte is read with the caller's credentials.
func TestServeRejectsRebindingHosts(t *testing.T) {
	r, _ := coretest.NewReader(t, rangetest.Data(1<<20), nil)
	handler := serveHandler(r, "data.bin", []string{"build-box.internal"})
	for host, want := range map[string]int{
		"127.0.0.1:8003":          http.StatusOK,
		"localhost:8003":          http.StatusOK,
		"LOCALHOST":               http.StatusOK,
		"[::1]:8003":              http.StatusOK,
		"10.0.0.7:8003":           http.StatusOK,
		"build-box.internal:8003": http.StatusOK,
		"evil.example:8003":       http.StatusForbidden,
		"evil.example":            http.StatusForbidden,
		"localhost.evil.example":  http.StatusForbidden,
		"127.0.0.1.nip.io:8003":   http.StatusForbidden,
		"":                        http.StatusForbidden,
	} {
		req := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
		req.Host = host
		req.Header.Set("Range", "bytes=0-9")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		got := rec.Code
		if got == http.StatusPartialContent {
			got = http.StatusOK
		}
		if got != want {
			t.Errorf("Host %q: status %d, want %d", host, rec.Code, want)
		}
	}
	fresh, counter := coretest.NewReader(t, rangetest.Data(1<<20), nil)
	req := httptest.NewRequest(http.MethodGet, "/data.bin", nil)
	req.Host = "evil.example"
	serveHandler(fresh, "data.bin", nil).ServeHTTP(httptest.NewRecorder(), req)
	if reads, _ := counter.Counts(); reads != 0 {
		t.Errorf("a refused request still made %d backend reads", reads)
	}
}

// range serve must behave like any HTTP object store to a client that reads
// by range: full GET, single and multi-range, HEAD, and conditional requests.
func TestServeHandlerSpeaksHTTPRange(t *testing.T) {
	data := rangetest.Data(3 << 20)
	r, counter := coretest.NewReader(t, data, nil)
	srv := httptest.NewServer(serveHandler(r, "data.bin", nil))
	defer srv.Close()
	get := func(header map[string]string) (*http.Response, []byte) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/data.bin", nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}

	resp, body := get(map[string]string{"Range": "bytes=1048570-1048585"})
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d, want 206", resp.StatusCode)
	}
	if !bytes.Equal(body, data[1048570:1048586]) {
		t.Fatal("a range across a block boundary returned the wrong bytes")
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" || resp.Header.Get("ETag") == "" {
		t.Errorf("missing Accept-Ranges or ETag: %v", resp.Header)
	}

	_, tail := get(map[string]string{"Range": "bytes=-100"})
	if !bytes.Equal(tail, data[len(data)-100:]) {
		t.Fatal("a suffix range returned the wrong bytes")
	}

	resp, full := get(nil)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(full, data) {
		t.Fatalf("full GET: status %d, %d bytes", resp.StatusCode, len(full))
	}
	etag := resp.Header.Get("ETag")

	resp, _ = get(map[string]string{"If-None-Match": etag})
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("If-None-Match with the current ETag = %d, want 304", resp.StatusCode)
	}

	head, err := http.Head(srv.URL + "/data.bin")
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.ContentLength != int64(len(data)) {
		t.Errorf("HEAD Content-Length = %d, want %d", head.ContentLength, len(data))
	}

	post, err := http.Post(srv.URL+"/data.bin", "text/plain", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", post.StatusCode)
	}

	// Having read everything once, reading it again costs nothing remote.
	before, _ := counter.Counts()
	get(nil)
	if after, _ := counter.Counts(); after != before {
		t.Errorf("a repeated full read went back to the backend %d times", after-before)
	}
}

func TestServedName(t *testing.T) {
	for uri, want := range map[string]string{
		"s3://bucket/data/events.parquet":                  "events.parquet",
		"https://host/path/db.sqlite?versionId=abc":        "db.sqlite",
		"/home/me/images/dev.range":                        "dev.range",
		"https://range-artifacts.s3.amazonaws.com/x.range": "x.range",
	} {
		if got := servedName(uri); got != want {
			t.Errorf("servedName(%q) = %q, want %q", uri, got, want)
		}
	}
}
