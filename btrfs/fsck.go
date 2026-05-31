package btrfs

import (
	"encoding/binary"
	"fmt"
	"strings"
)

type BtrfsFsckProblem struct {
	Severity string
	Message  string
}

type BtrfsFsckResult struct {
	NodesChecked uint32
	ItemsChecked uint32
	Problems     []BtrfsFsckProblem
	Healthy      bool
}

func (r *BtrfsFsckResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "B-Tree nodes checked:  %d\n", r.NodesChecked)
	fmt.Fprintf(&b, "Metadata items:        %d\n", r.ItemsChecked)
	if len(r.Problems) == 0 {
		fmt.Fprintf(&b, "\n✓ No problems found — Btrfs volume is healthy")
	} else {
		errors := 0
		warnings := 0
		for _, p := range r.Problems {
			if p.Severity == "error" {
				errors++
			} else {
				warnings++
			}
		}
		fmt.Fprintf(&b, "\nProblems: %d errors, %d warnings\n", errors, warnings)
		for _, p := range r.Problems {
			fmt.Fprintf(&b, "  [%s] %s\n", p.Severity, p.Message)
		}
		if r.Healthy {
			fmt.Fprintf(&b, "\n✓ Overall: healthy")
		} else {
			fmt.Fprintf(&b, "\n✗ Overall: unhealthy")
		}
	}
	return b.String()
}

func (b *FileSystem) Fsck() *BtrfsFsckResult {
	res := &BtrfsFsckResult{Healthy: true}

	if b.sb == nil {
		res.Healthy = false
		res.Problems = append(res.Problems, BtrfsFsckProblem{
			Severity: "error",
			Message:  "Btrfs superblock not loaded",
		})
		return res
	}

	if b.sb.Magic != btrfsMagic {
		res.Healthy = false
		res.Problems = append(res.Problems, BtrfsFsckProblem{
			Severity: "error",
			Message:  fmt.Sprintf("Invalid Btrfs magic: 0x%X (expected 0x%X)", b.sb.Magic, btrfsMagic),
		})
	}
	if b.sb.SectorSize != 4096 {
		res.Problems = append(res.Problems, BtrfsFsckProblem{
			Severity: "warning",
			Message:  fmt.Sprintf("Non-standard sector size: %d", b.sb.SectorSize),
		})
	}

	b.verifyTreeChecksums(b.sb.ChunkRoot, b.sb.ChunkRootLevel, res)
	b.verifyTreeChecksums(b.fsRoot, b.fsRootLvl, res)
	if b.extentRoot != 0 {
		b.verifyTreeChecksums(b.extentRoot, b.extentRootLvl, res)
	}

	return res
}

func (b *FileSystem) verifyTreeChecksums(addr uint64, level uint8, res *BtrfsFsckResult) {
	phys, err := b.tc.resolve(addr)
	if err != nil {
		res.Healthy = false
		res.Problems = append(res.Problems, BtrfsFsckProblem{
			Severity: "error",
			Message:  fmt.Sprintf("Failed to resolve logical address 0x%x: %v", addr, err),
		})
		return
	}

	buf, err := readNode(b.dev, phys, b.tc.nodeSize)
	if err != nil {
		res.Healthy = false
		res.Problems = append(res.Problems, BtrfsFsckProblem{
			Severity: "error",
			Message:  fmt.Sprintf("Failed to read node at physical address 0x%x: %v", phys, err),
		})
		return
	}

	res.NodesChecked++

	csumCalculated := calcCrc32c(buf[32:])
	csumExisting := binary.LittleEndian.Uint32(buf[0:4])
	if csumCalculated != csumExisting {
		res.Healthy = false
		res.Problems = append(res.Problems, BtrfsFsckProblem{
			Severity: "error",
			Message:  fmt.Sprintf("CRC32c checksum mismatch at logical 0x%x (phys 0x%x): got 0x%x, calculated 0x%x", addr, phys, csumExisting, csumCalculated),
		})
	}

	r := &byteReader{buf: buf}
	h := decodeNodeHeader(r)

	if h.level == 0 {
		res.ItemsChecked += h.nritems
		var prevKey key
		hasPrev := false
		for i := uint32(0); i < h.nritems; i++ {
			item := decodeLeafItem(r)
			if hasPrev {
				if compareKeys(item.key, prevKey) <= 0 {
					res.Healthy = false
					res.Problems = append(res.Problems, BtrfsFsckProblem{
						Severity: "error",
						Message:  fmt.Sprintf("Item key out of order at logical 0x%x, index %d", addr, i),
					})
				}
			}
			prevKey = item.key
			hasPrev = true
		}
	} else {
		for i := uint32(0); i < h.nritems; i++ {
			ptr := decodeInternalPtr(r)
			b.verifyTreeChecksums(ptr.blockptr, h.level-1, res)
		}
	}
}

func compareKeys(a, b key) int {
	if a.objectid < b.objectid {
		return -1
	} else if a.objectid > b.objectid {
		return 1
	}
	if a.typ < b.typ {
		return -1
	} else if a.typ > b.typ {
		return 1
	}
	if a.offset < b.offset {
		return -1
	} else if a.offset > b.offset {
		return 1
	}
	return 0
}
