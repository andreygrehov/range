package object

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Backend reads objects from S3 or an S3-compatible endpoint.
type S3Backend struct {
	Client *s3.Client
}

// NewS3 builds a client from the default AWS credential chain. A non-empty
// endpoint selects an S3-compatible service and path-style URLs.
func NewS3(endpoint string) (*S3Backend, error) {
	// WithEC2IMDSRegion falls back to the instance's own region when neither
	// the environment nor a config file names one, which is the common case
	// for a fresh EC2 host.
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithEC2IMDSRegion())
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("no AWS region configured; set AWS_REGION or run aws configure")
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
			o.UsePathStyle = true
		}
	})
	return &S3Backend{Client: client}, nil
}

// SplitS3URI splits s3://bucket/key.
func SplitS3URI(uri string) (bucket, key string, err error) {
	rest := strings.TrimPrefix(uri, "s3://")
	bucket, key, found := strings.Cut(rest, "/")
	if !found || bucket == "" || key == "" {
		return "", "", fmt.Errorf("invalid s3 uri %q, want s3://bucket/key", uri)
	}
	return bucket, key, nil
}

// Stat reports an object's size, ETag and version from a HEAD request.
func (b *S3Backend) Stat(ctx context.Context, uri string) (Info, error) {
	bucket, key, err := SplitS3URI(uri)
	if err != nil {
		return Info{}, err
	}
	out, err := b.Client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &bucket, Key: &key})
	if err != nil {
		return Info{}, fmt.Errorf("head %s: %w", uri, err)
	}
	info := Info{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ETag != nil {
		info.ETag = normalizeETag(*out.ETag)
	}
	if out.VersionId != nil {
		info.VersionID = *out.VersionId
	}
	if out.LastModified != nil {
		info.LastModified = *out.LastModified
	}
	return info, nil
}

// ReadRange fetches a byte range pinned to versionID, or to etag with
// If-Match when the bucket is unversioned.
func (b *S3Backend) ReadRange(ctx context.Context, uri string, offset, length int64, versionID, etag string) ([]byte, error) {
	bucket, key, err := SplitS3URI(uri)
	if err != nil {
		return nil, err
	}
	spec := fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	in := &s3.GetObjectInput{Bucket: &bucket, Key: &key, Range: &spec}
	switch {
	case versionID != "":
		in.VersionId = &versionID
	case etag != "":
		// Unversioned bucket: refuse bytes from a different generation rather
		// than silently splicing two versions of the object together.
		in.IfMatch = &etag
	}
	out, err := b.Client.GetObject(ctx, in)
	if err != nil {
		switch httpStatusOf(err) {
		case http.StatusPreconditionFailed:
			return nil, fmt.Errorf("%w: %s", ErrChanged, uri)
		case http.StatusForbidden, http.StatusNotFound:
			return nil, fmt.Errorf("get %s %s: %w: %w", uri, spec, errPermanent, err)
		}
		return nil, fmt.Errorf("get %s %s: %w", uri, spec, err)
	}
	defer out.Body.Close()
	return readFullRange(out.Body, length)
}
