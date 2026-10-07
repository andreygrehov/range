package ebs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ebs"
	"github.com/aws/aws-sdk-go-v2/service/ebs/types"
)

// fakeAPI serves a 1 GiB snapshot whose written blocks are in blocks, over
// two pages of listing.
type fakeAPI struct {
	blockSize int
	blocks    map[int32][]byte
	corrupt   int32 // a block whose checksum is wrong, or -1
	gets      atomic.Int64
}

func (f *fakeAPI) ListSnapshotBlocks(_ context.Context, in *ebs.ListSnapshotBlocksInput, _ ...func(*ebs.Options)) (*ebs.ListSnapshotBlocksOutput, error) {
	out := &ebs.ListSnapshotBlocksOutput{BlockSize: aws.Int32(int32(f.blockSize)), VolumeSize: aws.Int64(1)}
	page := aws.ToString(in.NextToken)
	for index := range f.blocks {
		// Even blocks on the first page, odd ones on the second.
		if (index%2 == 0) == (page == "") {
			out.Blocks = append(out.Blocks, types.Block{BlockIndex: aws.Int32(index), BlockToken: aws.String(fmt.Sprint("t", index))})
		}
	}
	if page == "" {
		out.NextToken = aws.String("second")
	}
	return out, nil
}

func (f *fakeAPI) GetSnapshotBlock(_ context.Context, in *ebs.GetSnapshotBlockInput, _ ...func(*ebs.Options)) (*ebs.GetSnapshotBlockOutput, error) {
	f.gets.Add(1)
	index := aws.ToInt32(in.BlockIndex)
	if aws.ToString(in.BlockToken) != fmt.Sprint("t", index) {
		return nil, fmt.Errorf("block %d asked for with token %q", index, aws.ToString(in.BlockToken))
	}
	data := f.blocks[index]
	sum := sha256.Sum256(data)
	if index == f.corrupt {
		sum[0]++
	}
	return &ebs.GetSnapshotBlockOutput{
		BlockData: io.NopCloser(bytes.NewReader(data)),
		Checksum:  aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	}, nil
}

func fill(b byte, n int) []byte { return bytes.Repeat([]byte{b}, n) }

func TestReadsAcrossBlocksWithZerosForUnwrittenOnes(t *testing.T) {
	f := &fakeAPI{blockSize: 1024, corrupt: -1, blocks: map[int32][]byte{
		0: fill('a', 1024), 1: fill('b', 1024), 3: fill('d', 1024),
	}}
	b := &Backend{client: f, snapshots: map[string]*snapshot{}}
	info, err := b.Stat(context.Background(), "ebs://snap-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 1<<30 || info.ETag != "snap-1" {
		t.Errorf("info = %+v", info)
	}
	before := f.gets.Load()
	got, err := b.ReadRange(context.Background(), "ebs://snap-1", 1000, 3100, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if n := f.gets.Load() - before; n != 3 {
		t.Errorf("fetched %d blocks for a read over three written ones and an unwritten one", n)
	}
	want := string(fill('a', 24)) + string(fill('b', 1024)) + string(fill(0, 1024)) + string(fill('d', 1024)) + string(fill(0, 4))
	if string(got) != want {
		t.Errorf("read the wrong bytes")
	}
	if _, err := b.ReadRange(context.Background(), "ebs://snap-1", 1<<30-10, 11, "", ""); err == nil {
		t.Error("a read past the end succeeded")
	}
}

func TestABlockThatFailsItsChecksumIsAnError(t *testing.T) {
	f := &fakeAPI{blockSize: 1024, corrupt: 2, blocks: map[int32][]byte{2: fill('c', 1024)}}
	b := &Backend{client: f, snapshots: map[string]*snapshot{}}
	_, err := b.ReadRange(context.Background(), "ebs://snap-1", 2048, 10, "", "")
	if err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("err = %v", err)
	}
}

func TestOnlySnapshotIDsAreURIs(t *testing.T) {
	b := &Backend{client: &fakeAPI{blockSize: 1024}, snapshots: map[string]*snapshot{}}
	for _, uri := range []string{"ebs://vol-1", "ebs://snap-1/2", "ebs://"} {
		if _, err := b.Stat(context.Background(), uri); err == nil {
			t.Errorf("%s opened", uri)
		}
	}
}

// gptDisk builds the first megabyte of a disk with a GPT partition table.
func gptDisk(types [][]byte, ranges [][2]uint64) []byte {
	head := make([]byte, 1<<20)
	head[510], head[511] = 0x55, 0xaa
	h := head[sector:]
	copy(h, "EFI PART")
	binary.LittleEndian.PutUint64(h[72:], 2) // entries from LBA 2
	binary.LittleEndian.PutUint32(h[80:], 128)
	binary.LittleEndian.PutUint32(h[84:], 128)
	for i := range types {
		e := head[2*sector+128*i:]
		copy(e, types[i])
		binary.LittleEndian.PutUint64(e[32:], ranges[i][0])
		binary.LittleEndian.PutUint64(e[40:], ranges[i][1])
	}
	return head
}

func TestFindPartition(t *testing.T) {
	efi := []byte{0x28, 0x73, 0x2a, 0xc1, 0x1f, 0xf8, 0xd2, 0x11, 0xba, 0x4b, 0x00, 0xa0, 0xc9, 0x3e, 0xc9, 0x3b}
	const size = 8 << 30
	// An AMI's layout: EFI, then a small Linux /boot, then the root.
	head := gptDisk([][]byte{efi, linuxData, linuxData}, [][2]uint64{{2048, 10239}, {10240, 20479}, {227328, 16777182}})
	p, err := findPartition(head, size)
	if err != nil || p != (partition{start: 227328 * sector, size: (16777182 - 227328 + 1) * sector}) {
		t.Errorf("GPT: %+v, %v", p, err)
	}
	if _, err := findPartition(gptDisk([][]byte{efi}, [][2]uint64{{2048, 4095}}), size); err == nil {
		t.Error("GPT without a Linux partition gave one")
	}
	// A partition that runs past the disk is not the disk's.
	if _, err := findPartition(gptDisk([][]byte{linuxData}, [][2]uint64{{2048, 1 << 40}}), size); err == nil {
		t.Error("a partition past the end was taken")
	}

	mbr := make([]byte, 1<<20)
	mbr[510], mbr[511] = 0x55, 0xaa
	e := mbr[446+16:]
	e[4] = 0x83
	binary.LittleEndian.PutUint32(e[8:], 2048)
	binary.LittleEndian.PutUint32(e[12:], 4096)
	if p, err := findPartition(mbr, size); err != nil || p != (partition{start: 2048 * sector, size: 4096 * sector}) {
		t.Errorf("MBR: %+v, %v", p, err)
	}

	if p, err := findPartition(make([]byte, 1<<20), size); err != nil || p != (partition{size: size}) {
		t.Errorf("no table: %+v, %v", p, err)
	}
}

func TestAnAMIOpensItsRootSnapshot(t *testing.T) {
	f := &fakeAPI{blockSize: 1024, corrupt: -1, blocks: map[int32][]byte{0: fill('a', 1024)}}
	b := &Backend{client: f, snapshots: map[string]*snapshot{},
		rootSnapshot: func(_ context.Context, ami string) (string, error) {
			if ami != "ami-7" {
				return "", fmt.Errorf("asked for %s", ami)
			}
			return "snap-9", nil
		}}
	info, err := b.Stat(context.Background(), "ebs://ami-7")
	if err != nil || info.ETag != "snap-9" {
		t.Errorf("info = %+v, %v", info, err)
	}
}

func TestParseRootSnapshot(t *testing.T) {
	answer := `<?xml version="1.0" encoding="UTF-8"?>
<DescribeImagesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <requestId>1</requestId>
  <imagesSet><item>
    <imageId>ami-1</imageId>
    <rootDeviceName>/dev/sda1</rootDeviceName>
    <blockDeviceMapping>
      <item><deviceName>/dev/sdb</deviceName><virtualName>ephemeral0</virtualName></item>
      <item><deviceName>/dev/sda1</deviceName><ebs><snapshotId>snap-root</snapshotId><volumeSize>8</volumeSize></ebs></item>
      <item><deviceName>/dev/sdf</deviceName><ebs><snapshotId>snap-data</snapshotId></ebs></item>
    </blockDeviceMapping>
  </item></imagesSet>
</DescribeImagesResponse>`
	if snap, err := parseRootSnapshot("ami-1", []byte(answer)); err != nil || snap != "snap-root" {
		t.Errorf("got %q, %v", snap, err)
	}
	empty := `<DescribeImagesResponse><imagesSet/></DescribeImagesResponse>`
	if _, err := parseRootSnapshot("ami-1", []byte(empty)); err == nil {
		t.Error("an empty answer gave a snapshot")
	}
	refused := `<Response><Errors><Error><Code>InvalidAMIID.Malformed</Code><Message>Invalid id: "ami-x"</Message></Error></Errors></Response>`
	if _, err := parseRootSnapshot("ami-x", []byte(refused)); err == nil || !strings.Contains(err.Error(), "InvalidAMIID.Malformed") {
		t.Errorf("error answer: %v", err)
	}
}
