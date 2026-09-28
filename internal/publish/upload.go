package publish

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/andreygrehov/range/internal/bytesize"
)

// Upload sends one file as an S3 multipart upload. Parts are read with
// ReadAt so they can go in parallel from a single handle, and a failure aborts
// the upload rather than leaving parts to be billed indefinitely.
func Upload(ctx context.Context, client *s3.Client, bucket, key string,
	file *os.File, size, partSize int64, parallel int) error {

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &bucket, Key: &key,
	})
	if err != nil {
		return fmt.Errorf("create multipart upload: %w", err)
	}
	uploadID := create.UploadId
	abort := func() {
		client.AbortMultipartUpload(context.Background(), &s3.AbortMultipartUploadInput{
			Bucket: &bucket, Key: &key, UploadId: uploadID,
		})
	}

	parts := (size + partSize - 1) / partSize
	if parts == 0 {
		parts = 1
	}
	if parts > 10000 {
		abort()
		return fmt.Errorf("publish: %s needs %d parts, more than S3 allows; raise --part-size",
			bytesize.Format(size), parts)
	}
	type completed struct {
		number int32
		etag   string
	}
	done := make([]completed, parts)
	var uploaded atomic.Int64
	group, groupCtx := newBoundedGroup(ctx, parallel)
	for index := int64(0); index < parts; index++ {
		offset := index * partSize
		length := partSize
		if remaining := size - offset; remaining < length {
			length = remaining
		}
		number := int32(index + 1)
		group.start(func() error {
			body := make([]byte, length)
			if _, err := file.ReadAt(body, offset); err != nil && err != io.EOF {
				return err
			}
			out, err := client.UploadPart(groupCtx, &s3.UploadPartInput{
				Bucket: &bucket, Key: &key, UploadId: uploadID,
				PartNumber: &number, Body: bytes.NewReader(body),
			})
			if err != nil {
				return fmt.Errorf("part %d: %w", number, err)
			}
			done[number-1] = completed{number: number, etag: *out.ETag}
			total := uploaded.Add(length)
			fmt.Fprintf(os.Stderr, "\r  %s / %s", bytesize.Format(total), bytesize.Format(size))
			return nil
		})
	}
	if err := group.wait(); err != nil {
		abort()
		return err
	}
	finished := make([]s3types.CompletedPart, 0, parts)
	for _, p := range done {
		number, etag := p.number, p.etag
		finished = append(finished, s3types.CompletedPart{PartNumber: &number, ETag: &etag})
	}
	if _, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &bucket, Key: &key, UploadId: uploadID,
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: finished},
	}); err != nil {
		abort()
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	return nil
}

// boundedGroup runs at most n workers and keeps the first error: enough of
// errgroup for one caller, without another dependency.
type boundedGroup struct {
	slots  chan struct{}
	wg     sync.WaitGroup
	mu     sync.Mutex
	err    error
	cancel context.CancelFunc
}

func newBoundedGroup(ctx context.Context, parallel int) (*boundedGroup, context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	return &boundedGroup{slots: make(chan struct{}, parallel), cancel: cancel}, ctx
}

func (g *boundedGroup) start(fn func() error) {
	g.slots <- struct{}{}
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() { <-g.slots }()
		if err := fn(); err != nil {
			g.mu.Lock()
			if g.err == nil {
				g.err = err
				g.cancel()
			}
			g.mu.Unlock()
		}
	}()
}

func (g *boundedGroup) wait() error {
	g.wg.Wait()
	g.cancel()
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.err
}
