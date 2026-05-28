package vfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"time"
)

func (fs *FileSystem) FreeBlock(blockNum uint64) error {
	sb := fs.sb
	blocksPerGroup := uint64(sb.S_blocks_per_group)
	groupIdx := uint32(blockNum / blocksPerGroup)
	localIndex := blockNum % blocksPerGroup

	if groupIdx >= fs.GroupCount {
		return fmt.Errorf("FreeBlock: block %d group %d out of range", blockNum, groupIdx)
	}

	bgd := &fs.Bgds[groupIdx]

	bitmapBlock := uint64(bgd.BG_block_bitmap_lo)
	if fs.DescSize > 32 {
		bitmapBlock |= uint64(bgd.BG_block_bitmap_hi) << 32
	}

	bitmap := make([]byte, fs.BlockSize)
	if _, err := fs.dev.ReadAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("FreeBlock: failed to read block bitmap for group %d: %w", groupIdx, err)
	}

	byteIdx := localIndex / 8
	bit := localIndex % 8

	if bitmap[byteIdx]&(1<<bit) == 0 {
		return fmt.Errorf("FreeBlock: block %d is already free", blockNum)
	}

	bitmap[byteIdx] &^= 1 << bit

	if _, err := fs.dev.WriteAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("FreeBlock: failed to write block bitmap for group %d: %w", groupIdx, err)
	}

	bgd.BG_free_blocks_count_lo++
	if err := fs.WriteGroupDescriptor(groupIdx, bgd); err != nil {
		return fmt.Errorf("FreeBlock: %w", err)
	}
	fs.updateSuperblockFreeBlocks(1)
	return nil
}

func (fs *FileSystem) FreeInode(inodeNum uint32) error {
	if inodeNum < 1 {
		return fmt.Errorf("FreeInode: invalid inode number %d", inodeNum)
	}

	sb := fs.sb
	inodesPerGroup := uint64(sb.S_inodes_per_group)
	groupIdx := uint32((uint64(inodeNum) - 1) / inodesPerGroup)
	localIndex := (uint64(inodeNum) - 1) % inodesPerGroup

	if groupIdx >= fs.GroupCount {
		return fmt.Errorf("FreeInode: inode %d group %d out of range", inodeNum, groupIdx)
	}

	bgd := &fs.Bgds[groupIdx]

	bitmapBlock := uint64(bgd.BG_inode_bitmap_lo)
	if fs.DescSize > 32 {
		bitmapBlock |= uint64(bgd.BG_inode_bitmap_hi) << 32
	}

	bitmap := make([]byte, fs.BlockSize)
	if _, err := fs.dev.ReadAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("FreeInode: failed to read inode bitmap for group %d: %w", groupIdx, err)
	}

	byteIdx := localIndex / 8
	bit := localIndex % 8

	if bitmap[byteIdx]&(1<<bit) == 0 {
		return fmt.Errorf("FreeInode: inode %d is already free", inodeNum)
	}

	bitmap[byteIdx] &^= 1 << bit

	if _, err := fs.dev.WriteAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("FreeInode: failed to write inode bitmap for group %d: %w", groupIdx, err)
	}

	bgd.BG_free_inodes_count_lo++
	if err := fs.WriteGroupDescriptor(groupIdx, bgd); err != nil {
		return fmt.Errorf("FreeInode: %w", err)
	}
	fs.updateSuperblockFreeInodes(1)
	return nil
}

func (fs *FileSystem) FreeExtents(extents []Extent) error {
	for _, ext := range extents {
		if ext.EE_len&0x8000 != 0 {
			continue
		}
		blockCount := uint64(ext.EE_len & 0x7FFF)
		physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
		for i := uint64(0); i < blockCount; i++ {
			if err := fs.FreeBlock(physStart + i); err != nil {
				return fmt.Errorf("FreeExtents: %w", err)
			}
		}
	}
	return nil
}

func (fs *FileSystem) TruncateInode(inode *Inode, newSize uint64) error {
	curSize := inode.Size()
	if newSize >= curSize {
		return nil
	}
	if !inode.UsesExtents() {
		return fmt.Errorf("TruncateInode: extents required")
	}

	blockSize := fs.BlockSize

	extents, err := fs.ReadExtents(inode)
	if err != nil {
		return fmt.Errorf("TruncateInode: failed to read extents: %w", err)
	}

	var kept []Extent

	if newSize == 0 {
		for _, ext := range extents {
			if ext.EE_len&0x8000 != 0 {
				continue
			}
			blockCount := uint64(ext.EE_len & 0x7FFF)
			physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
			for i := uint64(0); i < blockCount; i++ {
				if err := fs.FreeBlock(physStart + i); err != nil {
					return fmt.Errorf("TruncateInode: %w", err)
				}
			}
		}
	} else {
		newLastBlock := newSize / blockSize
		if newSize%blockSize == 0 {
			newLastBlock--
		}

		for _, ext := range extents {
			blockCount := uint64(ext.EE_len & 0x7FFF)
			startBlock := uint64(ext.EE_block)
			endBlock := startBlock + blockCount - 1

			if endBlock <= newLastBlock {
				kept = append(kept, ext)
				continue
			}
			if startBlock > newLastBlock {
				physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
				for i := uint64(0); i < blockCount; i++ {
					if err := fs.FreeBlock(physStart + i); err != nil {
						return fmt.Errorf("TruncateInode: %w", err)
					}
				}
				continue
			}
			keepCount := newLastBlock - startBlock + 1
			if keepCount > 0 {
				freeCount := blockCount - keepCount
				physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
				for i := uint64(0); i < freeCount; i++ {
					if err := fs.FreeBlock(physStart + keepCount + i); err != nil {
						return fmt.Errorf("TruncateInode: %w", err)
					}
				}
				ext.EE_len = ext.EE_len&0x8000 | uint16(keepCount)
				kept = append(kept, ext)
			}
		}
	}

	data := inode.I_block[:]
	var hdrBuf bytes.Buffer
	binary.Write(&hdrBuf, binary.LittleEndian, ExtentHeader{
		EH_magic:   EXT4_EXT_MAGIC,
		EH_entries: uint16(len(kept)),
		EH_max:     4,
		EH_depth:   0,
	})
	copy(data, hdrBuf.Bytes())

	for i, ext := range kept {
		off := extentHeaderSize + i*extentEntrySize
		if off+extentEntrySize > len(data) {
			break
		}
		var encBuf bytes.Buffer
		binary.Write(&encBuf, binary.LittleEndian, ext)
		copy(data[off:], encBuf.Bytes())
	}

	if inode.IsDir() {
		inode.I_size_lo = uint32(newSize)
	} else {
		inode.I_size_lo = uint32(newSize & 0xFFFFFFFF)
		inode.I_size_high = uint32(newSize >> 32)
	}
	newBlocks := (newSize + blockSize - 1) / blockSize
	sectorCount := newBlocks * (blockSize / 512)
	inode.I_blocks_lo = uint32(sectorCount & 0xFFFFFFFF)
	if sectorCount > 0xFFFFFFFF {
		inode.L_i_blocks_high = uint16(sectorCount >> 32)
	}
	inode.I_mtime = uint32(time.Now().Unix())
	inode.I_ctime = inode.I_mtime

	return nil
}

func (fs *FileSystem) updateSuperblockFreeBlocks(delta int64) {
	sb := fs.sb
	total := int64(uint64(sb.S_free_blocks_count_lo)) + delta
	if total < 0 {
		total = 0
	}
	sb.S_free_blocks_count_lo = uint32(total & 0xFFFFFFFF)
	fs.writeSuperBlock()
}

func (fs *FileSystem) updateSuperblockFreeInodes(delta int64) {
	sb := fs.sb
	total := int64(uint64(sb.S_free_inodes_count)) + delta
	if total < 0 {
		total = 0
	}
	sb.S_free_inodes_count = uint32(total & 0xFFFFFFFF)
	fs.writeSuperBlock()
}

func (fs *FileSystem) writeSuperBlock() {
	if fs.sb == nil {
		return
	}
	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, fs.sb); err != nil {
		log.Printf("[ext4] writeSuperBlock: encode error: %v", err)
		return
	}
	raw := buf.Bytes()
	if len(raw) > 1024 {
		raw = raw[:1024]
	}
	if _, err := fs.dev.WriteAt(raw, SUPERBLOCK_OFFSET); err != nil {
		log.Printf("[ext4] writeSuperBlock: write error: %v", err)
	}
}

func (fs *FileSystem) WriteSuperBlockPublic() {
	fs.writeSuperBlock()
}

func (fs *FileSystem) ClearSuperblockErrors() {
	if fs.sb == nil {
		return
	}
	fs.sb.S_state = SUPERBLOCK_STATE_CLEAN
	fs.writeSuperBlock()
	log.Printf("[ext4] superblock state cleared to CLEAN")
}
