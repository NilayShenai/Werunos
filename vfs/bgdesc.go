package vfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

type GroupDescriptor struct {

	BG_block_bitmap_lo uint32

	BG_inode_bitmap_lo uint32

	BG_inode_table_lo uint32

	BG_free_blocks_count_lo uint16

	BG_free_inodes_count_lo uint16

	BG_used_dirs_count_lo uint16

	BG_flags uint16

	BG_exclude_bitmap_lo uint32

	BG_block_bitmap_csum_lo uint16

	BG_inode_bitmap_csum_lo uint16

	BG_itable_unused_lo uint16

	BG_checksum uint16

	BG_block_bitmap_hi uint32

	BG_inode_bitmap_hi uint32

	BG_inode_table_hi uint32

	BG_free_blocks_count_hi uint16

	BG_free_inodes_count_hi uint16

	BG_used_dirs_count_hi uint16

	BG_itable_unused_hi uint16

	BG_exclude_bitmap_hi uint32

	BG_block_bitmap_csum_hi uint16

	BG_inode_bitmap_csum_hi uint16

	BG_reserved uint32
}

const (

	BG_INODE_UNINIT = 0x0001
	BG_BLOCK_UNINIT = 0x0002
	BG_INODE_ZEROED = 0x0004
)

func (fs *FileSystem) ReadGroupDescriptor(groupNum uint32) (*GroupDescriptor, error) {
	sb := fs.sb
	if sb == nil {
		return nil, fmt.Errorf("failed to read superblock")
	}

	if groupNum >= sb.BlockGroupCount() {
		return nil, fmt.Errorf("group number %d out of range (total groups: %d)", groupNum, sb.BlockGroupCount())
	}

	descSize := sb.GroupDescriptorSize()

	var descTableStart uint64
	if sb.BlockSize() == 1024 {
		descTableStart = 2048
	} else {
		descTableStart = sb.BlockSize()
	}
	descOffset := descTableStart + uint64(groupNum)*uint64(descSize)

	if descOffset > math.MaxInt64 {
		return nil, fmt.Errorf("descriptor offset %d overflows int64", descOffset)
	}

	buf := make([]byte, descSize)
	_, err := fs.dev.ReadAt(buf, int64(descOffset))
	if err != nil {
		return nil, fmt.Errorf("failed to read group descriptor: %w", err)
	}

	var gd GroupDescriptor
	err = binary.Read(bytes.NewReader(buf), binary.LittleEndian, &gd)
	if err != nil {
		return nil, fmt.Errorf("failed to decode group descriptor: %w", err)
	}

	return &gd, nil
}

func (fs *FileSystem) ReadGroupDescriptors() error {
	sb := fs.sb
	if sb == nil {
		return fmt.Errorf("superblock not read yet")
	}

	for i := range sb.BlockGroupCount() {
		gd, err := fs.ReadGroupDescriptor(i)
		if err != nil {
			return err
		}
		fs.Bgds = append(fs.Bgds, *gd)
	}

	return nil
}
