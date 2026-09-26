package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Info is what a backend knows about an object without reading it.
type Info struct {
	Size         int64
	ETag         string
	VersionID    string
	LastModified time.Time
}

// Backend is the lowest layer: it knows how to fetch a byte range and nothing else.
type Backend interface {
	Stat(ctx context.Context, uri string) (Info, error)
	ReadRange(ctx context.Context, uri string, offset, length int64, versionID, etag string) ([]byte, error)
}

// normalizeETag strips the quoting around a strong validator. A weak one
// (W/"...") is not usable with If-Match and must not be half-parsed into a
// value that silently fails every conditional request, so it is discarded.
func normalizeETag(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "W/") || strings.HasPrefix(value, "w/") {
		return ""
	}
	return strings.Trim(value, `"`)
}

// ErrChanged is returned when an object changed after it was opened, rather
// than splicing two generations of bytes together.
var ErrChanged = errors.New("range: remote object changed since it was opened")

// errPermanent marks a failure that retrying cannot fix: the object is gone, or
// these credentials will never be allowed to read it.
var errPermanent = errors.New("range: permanent error")

// httpStatusOf digs the HTTP status out of an AWS SDK or net/http error. The
// interface is matched structurally so no smithy import is needed, and a status
// code is checked rather than the error text: a request ID can contain "412".
func httpStatusOf(err error) int {
	var response interface{ HTTPStatusCode() int }
	if errors.As(err, &response) {
		return response.HTTPStatusCode()
	}
	return 0
}

// Retryable reports whether another attempt could plausibly succeed.
func Retryable(err error) bool {
	if errors.Is(err, ErrChanged) || errors.Is(err, errPermanent) {
		return false
	}
	switch httpStatusOf(err) {
	case http.StatusForbidden, http.StatusNotFound, http.StatusPreconditionFailed,
		http.StatusRequestedRangeNotSatisfiable:
		return false
	}
	return true
}

// Open returns the backend for a URI: s3://, http(s)://, file:// or a path.
func Open(uri, s3Endpoint string) (Backend, error) {
	switch {
	case strings.HasPrefix(uri, "s3://"):
		return NewS3(s3Endpoint)
	case strings.HasPrefix(uri, "http://"), strings.HasPrefix(uri, "https://"):
		return &httpBackend{client: &http.Client{Timeout: 60 * time.Second}}, nil
	case strings.HasPrefix(uri, "file://"):
		return FileBackend{}, nil
	default:
		return FileBackend{}, nil
	}
}

func readFullRange(body io.Reader, length int64) ([]byte, error) {
	buf := make([]byte, length)
	if _, err := io.ReadFull(body, buf); err != nil {
		return nil, fmt.Errorf("short read, wanted %d bytes: %w", length, err)
	}
	return buf, nil
}
