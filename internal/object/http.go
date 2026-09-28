package object

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

type httpBackend struct {
	client *http.Client
}

func (b *httpBackend) Stat(ctx context.Context, uri string) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, uri, nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return Info{}, fmt.Errorf("head %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Info{}, fmt.Errorf("head %s: unexpected status %s", uri, resp.Status)
	}
	if resp.ContentLength < 0 {
		return Info{}, fmt.Errorf("head %s: server did not report a content length", uri)
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Accept-Ranges")), "bytes") {
		return Info{}, fmt.Errorf("head %s: server does not advertise range support", uri)
	}
	info := Info{Size: resp.ContentLength, ETag: normalizeETag(resp.Header.Get("ETag"))}
	if value := resp.Header.Get("Last-Modified"); value != "" {
		if t, err := http.ParseTime(value); err == nil {
			info.LastModified = t
		}
	}
	return info, nil
}

func (b *httpBackend) ReadRange(ctx context.Context, uri string, offset, length int64, _, etag string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+length-1))
	if etag != "" {
		req.Header.Set("If-Match", `"`+etag+`"`)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", uri, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusPreconditionFailed {
		return nil, fmt.Errorf("%w: %s", ErrChanged, uri)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("get %s: want 206, got %s", uri, resp.Status)
	}
	return readFullRange(resp.Body, length)
}
