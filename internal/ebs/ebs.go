// Package ebs reads an EBS snapshot as a disk, through the EBS direct APIs,
// with no volume and no instance. ebs://snap-0123 names a snapshot, and
// ebs://ami-0123 the snapshot of an AMI's root device. Range shows the Linux filesystem on it: the snapshot's largest Linux
// partition, or the whole disk when it has no partition table.
//
// A snapshot never changes, so its ID is its identity. The API serves it in
// blocks of 512 KiB, with a SHA-256 for each, and leaves out every block that
// was never written: those read as zeros. Only snapshots this account owns,
// or that another account shared with it, can be read. Public snapshots,
// such as those of public AMIs, cannot: copy one first.
package ebs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ebs"

	"github.com/andreygrehov/range/internal/object"
)

// Scheme starts every URI this package reads.
const Scheme = "ebs://"

// IsURI reports whether uri names an EBS snapshot.
func IsURI(uri string) bool { return strings.HasPrefix(uri, Scheme) }

// parallel bounds the blocks one read fetches at once.
const parallel = 16

// api is the part of the EBS direct APIs Range uses.
type api interface {
	ListSnapshotBlocks(context.Context, *ebs.ListSnapshotBlocksInput, ...func(*ebs.Options)) (*ebs.ListSnapshotBlocksOutput, error)
	GetSnapshotBlock(context.Context, *ebs.GetSnapshotBlockInput, ...func(*ebs.Options)) (*ebs.GetSnapshotBlockOutput, error)
}

// Backend reads EBS snapshots.
type Backend struct {
	client api
	// rootSnapshot finds the snapshot behind an AMI.
	rootSnapshot func(ctx context.Context, ami string) (string, error)

	mu        sync.Mutex
	snapshots map[string]*snapshot
}

// snapshot is what one listing says about a snapshot: where its blocks are,
// and which part of it Range shows.
type snapshot struct {
	id        string
	blockSize int64
	tokens    map[int64]string // block index -> token; a missing block is zeros
	part      partition
}

// New builds a client from the default AWS credential chain.
func New() (*Backend, error) {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithEC2IMDSRegion())
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	if cfg.Region == "" {
		return nil, errors.New("no AWS region configured; set AWS_REGION or run aws configure")
	}
	return &Backend{
		client:       ebs.NewFromConfig(cfg),
		rootSnapshot: func(ctx context.Context, ami string) (string, error) { return rootSnapshot(ctx, cfg, ami) },
		snapshots:    map[string]*snapshot{},
	}, nil
}

// Stat lists the snapshot's blocks and finds its filesystem.
func (b *Backend) Stat(ctx context.Context, uri string) (object.Info, error) {
	s, err := b.open(ctx, uri)
	if err != nil {
		return object.Info{}, err
	}
	return object.Info{Size: s.part.size, ETag: s.id}, nil
}

// ReadRange reads the shown filesystem's bytes.
func (b *Backend) ReadRange(ctx context.Context, uri string, offset, length int64, _, _ string) ([]byte, error) {
	s, err := b.open(ctx, uri)
	if err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 || offset+length > s.part.size {
		return nil, fmt.Errorf("%s: read of %d bytes at %d is past its end", uri, length, offset)
	}
	return b.read(ctx, s, s.part.start+offset, length)
}

func (b *Backend) open(ctx context.Context, uri string) (*snapshot, error) {
	id := strings.TrimPrefix(uri, Scheme)
	if !(strings.HasPrefix(id, "snap-") || strings.HasPrefix(id, "ami-")) || strings.Contains(id, "/") {
		return nil, fmt.Errorf("%s: want ebs://snap-<id> or ebs://ami-<id>", uri)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.snapshots[id]; ok {
		return s, nil
	}
	snap := id
	if strings.HasPrefix(id, "ami-") {
		var err error
		if snap, err = b.rootSnapshot(ctx, id); err != nil {
			return nil, err
		}
	}
	s, err := b.list(ctx, snap)
	if err != nil {
		return nil, err
	}
	head, err := b.read(ctx, s, 0, min(s.part.size, 1<<20))
	if err != nil {
		return nil, err
	}
	if s.part, err = findPartition(head, s.part.size); err != nil {
		return nil, fmt.Errorf("%s: %w", uri, err)
	}
	// A kernel block device is whole 4 KiB blocks. A partition need only be
	// whole sectors, but its filesystem's blocks are 4 KiB, so the part of a
	// block at its end holds nothing.
	s.part.size &^= 4095
	b.snapshots[id] = s
	return s, nil
}

// list reads the snapshot's block list. The tokens it returns stay valid for
// days, far longer than a session.
func (b *Backend) list(ctx context.Context, id string) (*snapshot, error) {
	s := &snapshot{id: id, tokens: map[int64]string{}}
	in := &ebs.ListSnapshotBlocksInput{SnapshotId: &id, MaxResults: aws.Int32(10000)}
	for {
		out, err := b.client.ListSnapshotBlocks(ctx, in)
		if err != nil && strings.Contains(err.Error(), "Public snapshots are not supported") {
			return nil, fmt.Errorf("%s is public, and AWS serves only snapshots this account owns "+
				"or that are shared with it; copy it first with aws ec2 copy-snapshot", id)
		}
		if err != nil {
			return nil, fmt.Errorf("list the blocks of %s: %w", id, err)
		}
		s.blockSize = int64(aws.ToInt32(out.BlockSize))
		s.part.size = aws.ToInt64(out.VolumeSize) << 30
		for _, block := range out.Blocks {
			s.tokens[int64(aws.ToInt32(block.BlockIndex))] = aws.ToString(block.BlockToken)
		}
		if out.NextToken == nil {
			break
		}
		in.NextToken = out.NextToken
	}
	if s.blockSize <= 0 || s.part.size <= 0 {
		return nil, fmt.Errorf("%s: the EBS API gave no size", id)
	}
	return s, nil
}

// read reads length bytes at offset on the whole disk, a block at a time,
// several at once.
func (b *Backend) read(ctx context.Context, s *snapshot, offset, length int64) ([]byte, error) {
	out := make([]byte, length)
	first, last := offset/s.blockSize, (offset+length-1)/s.blockSize
	errs := make(chan error, last-first+1)
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := first; i <= last; i++ {
		token, written := s.tokens[i]
		if !written {
			continue // out is already zeros
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			data, err := b.block(ctx, s, i, token)
			if err != nil {
				errs <- err
				return
			}
			start := i * s.blockSize
			from, to := max(offset, start), min(offset+length, start+int64(len(data)))
			if from < to {
				copy(out[from-offset:to-offset], data[from-start:to-start])
			}
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return nil, err
	}
	return out, nil
}

// block fetches one block and checks it against its SHA-256.
func (b *Backend) block(ctx context.Context, s *snapshot, index int64, token string) ([]byte, error) {
	out, err := b.client.GetSnapshotBlock(ctx, &ebs.GetSnapshotBlockInput{
		SnapshotId: &s.id, BlockIndex: aws.Int32(int32(index)), BlockToken: &token,
	})
	if err != nil {
		return nil, fmt.Errorf("read block %d of %s: %w", index, s.id, err)
	}
	defer out.BlockData.Close()
	data, err := io.ReadAll(out.BlockData)
	if err != nil {
		return nil, fmt.Errorf("read block %d of %s: %w", index, s.id, err)
	}
	sum := sha256.Sum256(data)
	if want, err := base64.StdEncoding.DecodeString(aws.ToString(out.Checksum)); err != nil || !bytes.Equal(want, sum[:]) {
		return nil, fmt.Errorf("block %d of %s does not match its SHA-256", index, s.id)
	}
	return data, nil
}
