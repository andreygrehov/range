package ebs

import (
	"bytes"
	"encoding/binary"
	"errors"
)

// partition is a byte range of a disk.
type partition struct {
	start, size int64
}

const sector = 512

// linuxData is the GPT type of a Linux filesystem partition, as it is stored:
// 0FC63DAF-8483-4772-8E79-3D69D8477DE4, the first three fields little-endian.
var linuxData = []byte{0xaf, 0x3d, 0xc6, 0x0f, 0x83, 0x84, 0x72, 0x47,
	0x8e, 0x79, 0x3d, 0x69, 0xd8, 0x47, 0x7d, 0xe4}

// findPartition returns the largest Linux partition of a disk of size bytes
// whose first bytes are head, or the whole disk when it has no partition
// table. head must hold the partition table: GPT keeps it in the first
// sectors, and AMIs keep it in the first megabyte.
func findPartition(head []byte, size int64) (partition, error) {
	whole := partition{start: 0, size: size}
	if len(head) < 2*sector || head[510] != 0x55 || head[511] != 0xaa {
		return whole, nil
	}
	if string(head[sector:sector+8]) == "EFI PART" {
		return gptPartition(head, size)
	}
	// MBR: four entries of 16 bytes from 446. Type 0x83 is Linux.
	var best partition
	for i := 0; i < 4; i++ {
		e := head[446+16*i:]
		start := int64(binary.LittleEndian.Uint32(e[8:])) * sector
		n := int64(binary.LittleEndian.Uint32(e[12:])) * sector
		if e[4] == 0x83 && n > best.size && start+n <= size {
			best = partition{start: start, size: n}
		}
	}
	if best.size == 0 {
		return partition{}, errors.New("its MBR partition table has no Linux partition")
	}
	return best, nil
}

func gptPartition(head []byte, size int64) (partition, error) {
	h := head[sector:]
	entries := int64(binary.LittleEndian.Uint64(h[72:])) * sector
	count := int64(binary.LittleEndian.Uint32(h[80:]))
	entrySize := int64(binary.LittleEndian.Uint32(h[84:]))
	if entrySize < 128 || count > 1024 || entries+count*entrySize > int64(len(head)) {
		return partition{}, errors.New("its GPT partition table is not where Range can read it")
	}
	var best partition
	for i := int64(0); i < count; i++ {
		e := head[entries+i*entrySize:]
		if !bytes.Equal(e[:16], linuxData) {
			continue
		}
		first := int64(binary.LittleEndian.Uint64(e[32:]))
		last := int64(binary.LittleEndian.Uint64(e[40:]))
		p := partition{start: first * sector, size: (last - first + 1) * sector}
		if last >= first && p.size > best.size && p.start+p.size <= size {
			best = p
		}
	}
	if best.size == 0 {
		return partition{}, errors.New("its GPT partition table has no Linux partition")
	}
	return best, nil
}
