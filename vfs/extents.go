package vfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

type ExtentHeader struct {
	EH_magic      uint16
	EH_entries    uint16
	EH_max        uint16
	EH_depth      uint16
	EH_generation uint32
}

type Extent struct {
	EE_block    uint32
	EE_len      uint16
	EE_start_hi uint16
	EE_start_lo uint32
}

type ExtentIdx struct {
	EI_block   uint32
	EI_leaf_lo uint32
	EI_leaf_hi uint16
	EI_unused  uint16
}

const EXT4_EXT_MAGIC uint16 = 0xF30A

const (
	extentHeaderSize = 12
	extentEntrySize  = 12
)

func (fs *FileSystem) ReadExtents(inode *Inode) ([]Extent, error) {

	if !inode.UsesExtents() {
		return nil, fmt.Errorf(
			"inode uses legacy indirect block addressing (EXT4_EXTENTS_FL not set in I_flags=0x%08x); "+
				"indirect block scheme is not yet supported",
			inode.I_flags,
		)
	}

	return fs.parseExtentNode(inode.I_block[:])
}

func (fs *FileSystem) parseExtentNode(data []byte) ([]Extent, error) {
	if len(data) < extentHeaderSize {
		return nil, fmt.Errorf("extent node too small: %d bytes (need at least %d)", len(data), extentHeaderSize)
	}

	var hdr ExtentHeader
	if err := binary.Read(bytes.NewReader(data[:extentHeaderSize]), binary.LittleEndian, &hdr); err != nil {
		return nil, fmt.Errorf("failed to decode extent header: %w", err)
	}

	if hdr.EH_magic != EXT4_EXT_MAGIC {
		return nil, fmt.Errorf("bad extent magic: expected 0x%04X, got 0x%04X", EXT4_EXT_MAGIC, hdr.EH_magic)
	}

	if hdr.EH_depth == 0 {

		return parseLeafEntries(data[extentHeaderSize:], hdr.EH_entries)
	}

	var all []Extent

	for i := range uint16(hdr.EH_entries) {
		entryOffset := extentHeaderSize + int(i)*extentEntrySize
		if entryOffset+extentEntrySize > len(data) {
			return nil, fmt.Errorf("extent index %d extends beyond node data", i)
		}

		var idx ExtentIdx
		if err := binary.Read(
			bytes.NewReader(data[entryOffset:entryOffset+extentEntrySize]),
			binary.LittleEndian, &idx,
		); err != nil {
			return nil, fmt.Errorf("failed to decode extent index %d: %w", i, err)
		}

		childBlock := (uint64(idx.EI_leaf_hi) << 32) | uint64(idx.EI_leaf_lo)
		childOffset := int64(childBlock * fs.BlockSize)

		childBuf := make([]byte, fs.BlockSize)
		if _, err := fs.dev.ReadAt(childBuf, childOffset); err != nil {
			return nil, fmt.Errorf(
				"failed to read extent index block %d (offset %d): %w",
				childBlock, childOffset, err,
			)
		}

		childExtents, err := fs.parseExtentNode(childBuf)
		if err != nil {
			return nil, fmt.Errorf("failed to parse child extent node at block %d: %w", childBlock, err)
		}
		all = append(all, childExtents...)
	}

	return all, nil
}

func parseLeafEntries(data []byte, count uint16) ([]Extent, error) {
	extents := make([]Extent, 0, count)

	for i := range uint16(count) {
		offset := int(i) * extentEntrySize
		if offset+extentEntrySize > len(data) {
			return nil, fmt.Errorf("extent entry %d extends beyond leaf node data", i)
		}

		var ext Extent
		if err := binary.Read(
			bytes.NewReader(data[offset:offset+extentEntrySize]),
			binary.LittleEndian, &ext,
		); err != nil {
			return nil, fmt.Errorf("failed to decode extent entry %d: %w", i, err)
		}
		extents = append(extents, ext)
	}

	return extents, nil
}
