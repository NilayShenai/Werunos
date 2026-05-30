package btrfs

import (
	"fmt"
)

const (
	sysChunkArrayOffset = 811
	sysChunkArrayMax    = 2048
)

func ReadSuperBlock(dev readerAt) (*SuperBlock, error) {
	buf := make([]byte, superblockSize)
	_, err := dev.ReadAt(buf, superblockOffset)
	if err != nil {
		return nil, fmt.Errorf("btrfs: read superblock: %w", err)
	}
	var sb SuperBlock
	r := &byteReader{buf: buf}
	r.copy(sb.Csum[:], 32)
	r.copy(sb.Fsid[:], 16)
	sb.Bytenr = r.u64()
	sb.Flags = r.u64()
	sb.Magic = r.u64()
	if sb.Magic != btrfsMagic {
		return nil, fmt.Errorf("invalid btrfs magic: expected 0x%X, got 0x%X", btrfsMagic, sb.Magic)
	}
	sb.Generation = r.u64()
	sb.Root = r.u64()
	sb.ChunkRoot = r.u64()
	sb.LogRoot = r.u64()
	sb.LogRootTransid = r.u64()
	sb.TotalBytes = r.u64()
	sb.BytesUsed = r.u64()
	sb.RootDirObjectid = r.u64()
	sb.NumDevices = r.u64()
	sb.SectorSize = r.u32()
	sb.NodeSize = r.u32()
	sb.LeafSize = r.u32()
	sb.StripeSize = r.u32()
	sb.SysChunkArraySize = r.u32()
	sb.ChunkRootGeneration = r.u64()
	sb.CompatFlags = r.u64()
	sb.CompatRoFlags = r.u64()
	sb.IncompatFlags = r.u64()
	sb.CsumType = r.u16()
	sb.RootLevel = r.u8()
	sb.ChunkRootLevel = r.u8()
	sb.LogRootLevel = r.u8()

	asz := int(sb.SysChunkArraySize)
	if asz > 0 && asz <= sysChunkArrayMax {
		end := sysChunkArrayOffset + asz
		if end <= len(buf) {
			sb.SysChunkData = make([]byte, asz)
			copy(sb.SysChunkData, buf[sysChunkArrayOffset:end])
		}
	}
	return &sb, nil
}

func (sb *SuperBlock) parseSysChunks(tc *treeContext) error {
	data := sb.SysChunkData
	if len(data) == 0 {
		return nil
	}
	pos := 0
	for pos+17 <= len(data) {
		kr := &byteReader{buf: data[pos:]}
		k := decodeKey(kr)
		pos += 17

		if k.typ != BTRFS_CHUNK_ITEM_KEY || pos+48 > len(data) {
			break
		}

		cr := &byteReader{buf: data[pos:]}
		length := cr.u64()
		_ = cr.u64() // owner
		_ = cr.u64() // stripe_len
		flags := cr.u64() // type (flags)
		_ = cr.u32() // io_align
		_ = cr.u32() // io_width
		_ = cr.u32() // sector_size
		numStripes := cr.u16()
		cr.skip(2) // sub_stripes

		if numStripes == 0 {
			break
		}

		stripeTotal := int(numStripes) * 32
		if pos+48+stripeTotal > len(data) {
			break
		}

		logical := k.offset
		lastStripeOff := pos + 48 + stripeTotal - 32
		sr := &byteReader{buf: data[lastStripeOff:]}
		sr.skip(8) // device_id
		physOffset := sr.u64()
		tc.addChunk(logical, length, physOffset, flags)

		pos += 48 + stripeTotal
	}
	return nil
}

type SuperBlock struct {
	Csum                [32]byte
	Fsid                [16]byte
	Bytenr              uint64
	Flags               uint64
	Magic               uint64
	Generation          uint64
	Root                uint64
	ChunkRoot           uint64
	LogRoot             uint64
	LogRootTransid      uint64
	TotalBytes          uint64
	BytesUsed           uint64
	RootDirObjectid     uint64
	NumDevices          uint64
	SectorSize          uint32
	NodeSize            uint32
	LeafSize            uint32
	StripeSize          uint32
	SysChunkArraySize   uint32
	ChunkRootGeneration uint64
	CompatFlags         uint64
	CompatRoFlags       uint64
	IncompatFlags       uint64
	CsumType            uint16
	RootLevel           uint8
	ChunkRootLevel      uint8
	LogRootLevel        uint8
	SysChunkData        []byte
}
