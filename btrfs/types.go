package btrfs

import (
	"encoding/binary"
)

type readerAt interface {
	ReadAt(p []byte, off int64) (int, error)
}

type byteReader struct {
	buf []byte
	off int
}

func (r *byteReader) u8() uint8 {
	v := r.buf[r.off]
	r.off++
	return v
}

func (r *byteReader) u16() uint16 {
	v := binary.LittleEndian.Uint16(r.buf[r.off:])
	r.off += 2
	return v
}

func (r *byteReader) u32() uint32 {
	v := binary.LittleEndian.Uint32(r.buf[r.off:])
	r.off += 4
	return v
}

func (r *byteReader) u64() uint64 {
	v := binary.LittleEndian.Uint64(r.buf[r.off:])
	r.off += 8
	return v
}

func (r *byteReader) copy(dst []byte, n int) {
	copy(dst, r.buf[r.off:r.off+n])
	r.off += n
}

func (r *byteReader) skip(n int) {
	r.off += n
}

func readAt(dev readerAt, buf []byte, off uint64) error {
	_, err := dev.ReadAt(buf, int64(off))
	return err
}

const (
	nodeHeaderSize = 101

	btrfsMagic = 0x4D5F53665248425F

	superblockOffset = 0x10000
	superblockSize   = 4096

	BTRFS_ROOT_ITEM_KEY    = 132
	BTRFS_DIR_ITEM_KEY     = 84
	BTRFS_DIR_INDEX_KEY    = 96
	BTRFS_INODE_ITEM_KEY   = 1
	BTRFS_INODE_REF_KEY    = 12
	BTRFS_EXTENT_DATA_KEY  = 108
	BTRFS_CHUNK_ITEM_KEY   = 228
	BTRFS_ROOT_BACKREF_KEY = 144
	BTRFS_EXTENT_ITEM_KEY  = 168
	BTRFS_BLOCK_GROUP_ITEM_KEY = 192

	BTRFS_FILE_EXTENT_INLINE  = 0
	BTRFS_FILE_EXTENT_REG     = 1
	BTRFS_FILE_EXTENT_PREALLOC = 2

	BTRFS_BLOCK_GROUP_DATA     = 1
	BTRFS_BLOCK_GROUP_SYSTEM   = 2
	BTRFS_BLOCK_GROUP_METADATA = 4

	BTRFS_FT_UNKNOWN  = 0
	BTRFS_FT_REG_FILE = 1
	BTRFS_FT_DIR      = 2
	BTRFS_FT_SYMLINK  = 7
)

type key struct {
	objectid uint64
	typ      uint8
	offset   uint64
}

func (k key) less(other key) bool {
	if k.objectid != other.objectid {
		return k.objectid < other.objectid
	}
	if k.typ != other.typ {
		return k.typ < other.typ
	}
	return k.offset < other.offset
}

func decodeKey(r *byteReader) key {
	return key{
		objectid: r.u64(),
		typ:      r.u8(),
		offset:   r.u64(),
	}
}

type nodeHeader struct {
	csum      [32]byte
	fsid      [16]byte
	bytenr    uint64
	flags     uint64
	chunkUuid [16]byte
	generation uint64
	owner     uint64
	nritems   uint32
	level     uint8
}

func decodeNodeHeader(r *byteReader) nodeHeader {
	var h nodeHeader
	r.copy(h.csum[:], 32)
	r.copy(h.fsid[:], 16)
	h.bytenr = r.u64()
	h.flags = r.u64()
	r.copy(h.chunkUuid[:], 16)
	h.generation = r.u64()
	h.owner = r.u64()
	h.nritems = r.u32()
	h.level = r.u8()
	return h
}

type leafItem struct {
	key    key
	offset uint32
	size   uint32
}

func decodeLeafItem(r *byteReader) leafItem {
	return leafItem{
		key:    decodeKey(r),
		offset: r.u32(),
		size:   r.u32(),
	}
}

type internalKeyPtr struct {
	key        key
	blockptr   uint64
	generation uint64
}

func decodeInternalPtr(r *byteReader) internalKeyPtr {
	return internalKeyPtr{
		key:        decodeKey(r),
		blockptr:   r.u64(),
		generation: r.u64(),
	}
}

func readNode(dev readerAt, addr uint64, nodeSize uint32) ([]byte, error) {
	buf := make([]byte, nodeSize)
	if err := readAt(dev, buf, addr); err != nil {
		return nil, err
	}
	return buf, nil
}
