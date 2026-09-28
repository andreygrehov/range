package object

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// FileBackend reads local files, by path or file:// URI.
type FileBackend struct{}

func filePath(uri string) (string, error) {
	if !strings.HasPrefix(uri, "file://") {
		return uri, nil
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("invalid file uri %q: %w", uri, err)
	}
	return parsed.Path, nil
}

// Stat reports a file's size and modification time.
func (FileBackend) Stat(_ context.Context, uri string) (Info, error) {
	path, err := filePath(uri)
	if err != nil {
		return Info{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	if info.IsDir() {
		return Info{}, fmt.Errorf("%s is a directory", path)
	}
	modified := info.ModTime()
	return Info{
		Size:         info.Size(),
		ETag:         fmt.Sprintf("%d-%d", info.Size(), modified.UnixNano()),
		LastModified: modified,
	}, nil
}

// ReadRange reads a byte range of the file.
func (FileBackend) ReadRange(_ context.Context, uri string, offset, length int64, _, _ string) ([]byte, error) {
	path, err := filePath(uri)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, length)
	if _, err := io.ReadFull(io.NewSectionReader(f, offset, length), buf); err != nil {
		return nil, fmt.Errorf("read %s at %d: %w", path, offset, err)
	}
	return buf, nil
}
