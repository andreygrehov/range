package artifact

import (
	"context"

	"github.com/andreygrehov/range/internal/object"
)

// shortBackend answers every range request with one byte fewer than asked, the
// way a truncated or misbehaving server might.
type shortBackend struct{ object.FileBackend }

func (s shortBackend) ReadRange(ctx context.Context, uri string, offset, length int64, v, e string) ([]byte, error) {
	b, err := s.FileBackend.ReadRange(ctx, uri, offset, length, v, e)
	if err == nil && len(b) > 0 && offset >= headerSize {
		b = b[:len(b)-1]
	}
	return b, err
}
