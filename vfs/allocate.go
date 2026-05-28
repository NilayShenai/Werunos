package vfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"time"
)

func (fs *FileSystem) AllocateBlock() (uint64, error) {
	sb := fs.sb
	blocksPerGroup := uint64(sb.S_blocks_per_group)

	for groupIdx := uint32(0); groupIdx < fs.GroupCount; groupIdx++ {
		bgd := &fs.Bgds[groupIdx]

		freeBlocks := uint64(bgd.BG_free_blocks_count_lo)
		if fs.DescSize > 32 {
			freeBlocks |= uint64(bgd.BG_free_blocks_count_hi) << 16
		}
		if freeBlocks == 0 {
			continue
		}

		bitmapBlock := uint64(bgd.BG_block_bitmap_lo)
		if fs.DescSize > 32 {
			bitmapBlock |= uint64(bgd.BG_block_bitmap_hi) << 32
		}

		bitmap := make([]byte, fs.BlockSize)
		if _, err := fs.dev.ReadAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
			return 0, fmt.Errorf("AllocateBlock: failed to read block bitmap for group %d: %w", groupIdx, err)
		}

		for byteIdx := range bitmap {
			if bitmap[byteIdx] == 0xFF {
				continue
			}
			for bit := uint(0); bit < 8; bit++ {
				if bitmap[byteIdx]&(1<<bit) == 0 {
					bitmap[byteIdx] |= 1 << bit

					if _, err := fs.dev.WriteAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
						return 0, fmt.Errorf("AllocateBlock: failed to write block bitmap for group %d: %w", groupIdx, err)
					}

					blockNum := uint64(groupIdx)*blocksPerGroup + uint64(byteIdx)*8 + uint64(bit)

					bgd.BG_free_blocks_count_lo--
					if err := fs.WriteGroupDescriptor(groupIdx, bgd); err != nil {
						return 0, fmt.Errorf("AllocateBlock: %w", err)
					}
					fs.updateSuperblockFreeBlocks(-1)

					return blockNum, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("AllocateBlock: no free blocks available")
}

func (fs *FileSystem) WriteInode(inodeNum uint32, inode *Inode) error {
	if inodeNum < 1 {
		return fmt.Errorf("WriteInode: invalid inode number: %d", inodeNum)
	}

	groupIndex := (inodeNum - 1) / fs.sb.S_inodes_per_group
	localInodeIndex := (inodeNum - 1) % fs.sb.S_inodes_per_group

	if groupIndex >= fs.GroupCount {
		return fmt.Errorf("WriteInode: inode %d group %d out of range", inodeNum, groupIndex)
	}

	bgd := fs.Bgds[groupIndex]
	tableBlock := uint64(bgd.BG_inode_table_lo)
	if fs.DescSize > 32 {
		tableBlock |= uint64(bgd.BG_inode_table_hi) << 32
	}

	offset := int64(tableBlock*fs.BlockSize) + int64(localInodeIndex)*int64(fs.InodeSize)

	raw := make([]byte, fs.InodeSize)
	if _, err := fs.dev.ReadAt(raw, offset); err != nil {
		return fmt.Errorf("WriteInode: failed to read current inode %d data: %w", inodeNum, err)
	}

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, inode); err != nil {
		return fmt.Errorf("WriteInode: failed to encode inode %d: %w", inodeNum, err)
	}
	inodeBytes := buf.Bytes()
	if len(inodeBytes) > int(fs.InodeSize) {
		inodeBytes = inodeBytes[:fs.InodeSize]
	}
	copy(raw, inodeBytes)

	if _, err := fs.dev.WriteAt(raw, offset); err != nil {
		return fmt.Errorf("WriteInode: failed to write inode %d: %w", inodeNum, err)
	}
	return nil
}

func (fs *FileSystem) AppendExtent(inode *Inode, newExt Extent) error {
	if !inode.UsesExtents() {
		return fmt.Errorf("AppendExtent: inode does not use extents")
	}

	var hdr ExtentHeader
	data := inode.I_block[:]
	if err := binary.Read(bytes.NewReader(data[:extentHeaderSize]), binary.LittleEndian, &hdr); err != nil {
		return fmt.Errorf("AppendExtent: failed to decode header: %w", err)
	}

	if hdr.EH_magic != EXT4_EXT_MAGIC {
		return fmt.Errorf("AppendExtent: bad extent magic: 0x%04x", hdr.EH_magic)
	}
	if hdr.EH_depth != 0 {
		return fmt.Errorf("AppendExtent: only depth-0 extent trees are supported (got depth=%d)", hdr.EH_depth)
	}
	if hdr.EH_entries >= hdr.EH_max {
		return fmt.Errorf("AppendExtent: extent tree is full (%d entries, max %d)", hdr.EH_entries, hdr.EH_max)
	}

	entries := hdr.EH_entries

	if entries > 0 {
		lastOff := extentHeaderSize + int(entries-1)*extentEntrySize
		var lastExt Extent
		if err := binary.Read(bytes.NewReader(data[lastOff:lastOff+extentEntrySize]), binary.LittleEndian, &lastExt); err != nil {
			return fmt.Errorf("AppendExtent: failed to decode last extent: %w", err)
		}

		lastBlockEnd := uint64(lastExt.EE_block) + uint64(lastExt.EE_len&0x7FFF)
		lastPhysEnd := uint64(lastExt.EE_start_lo) + uint64(lastExt.EE_len&0x7FFF)
		newPhysStart := uint64(newExt.EE_start_lo)
		if lastExt.EE_start_hi == newExt.EE_start_hi &&
			lastBlockEnd == uint64(newExt.EE_block) &&
			lastPhysEnd == newPhysStart &&
			(lastExt.EE_len&0x8000) == 0 && (newExt.EE_len&0x8000) == 0 {
			combinedLen := uint64(lastExt.EE_len&0x7FFF) + uint64(newExt.EE_len&0x7FFF)
			if combinedLen <= 0x7FFF {
				lastExt.EE_len = lastExt.EE_len&0x8000 | uint16(combinedLen)
				var encBuf bytes.Buffer
				if err := binary.Write(&encBuf, binary.LittleEndian, lastExt); err != nil {
					return fmt.Errorf("AppendExtent: failed to encode merged extent: %w", err)
				}
				copy(data[lastOff:], encBuf.Bytes())
				inode.I_mtime = uint32(time.Now().Unix())
				return nil
			}
		}
	}

	entryOff := extentHeaderSize + int(entries)*extentEntrySize
	var encBuf bytes.Buffer
	if err := binary.Write(&encBuf, binary.LittleEndian, newExt); err != nil {
		return fmt.Errorf("AppendExtent: failed to encode new extent: %w", err)
	}
	copy(data[entryOff:], encBuf.Bytes())

	hdr.EH_entries++
	var hdrBuf bytes.Buffer
	if err := binary.Write(&hdrBuf, binary.LittleEndian, hdr); err != nil {
		return fmt.Errorf("AppendExtent: failed to encode header: %w", err)
	}
	copy(data[:extentHeaderSize], hdrBuf.Bytes())

	inode.I_mtime = uint32(time.Now().Unix())
	return nil
}

func (fs *FileSystem) AllocateInode() (uint32, error) {
	sb := fs.sb
	inodesPerGroup := uint64(sb.S_inodes_per_group)

	for groupIdx := uint32(0); groupIdx < fs.GroupCount; groupIdx++ {
		bgd := &fs.Bgds[groupIdx]

		freeInodes := uint64(bgd.BG_free_inodes_count_lo)
		if fs.DescSize > 32 {
			freeInodes |= uint64(bgd.BG_free_inodes_count_hi) << 16
		}
		if freeInodes == 0 {
			continue
		}

		bitmapBlock := uint64(bgd.BG_inode_bitmap_lo)
		if fs.DescSize > 32 {
			bitmapBlock |= uint64(bgd.BG_inode_bitmap_hi) << 32
		}

		bitmap := make([]byte, fs.BlockSize)
		if _, err := fs.dev.ReadAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
			return 0, fmt.Errorf("AllocateInode: failed to read inode bitmap for group %d: %w", groupIdx, err)
		}

		startBit := uint64(0)
		if groupIdx == 0 {
			startBit = 11
		}

		for byteIdx := startBit / 8; byteIdx < fs.BlockSize; byteIdx++ {
			if bitmap[byteIdx] == 0xFF {
				continue
			}
			minBit := uint64(0)
			if byteIdx == startBit/8 {
				minBit = startBit % 8
			}
			for bit := minBit; bit < 8; bit++ {
				if bitmap[byteIdx]&(1<<bit) == 0 {
					bitmap[byteIdx] |= 1 << bit

					if _, err := fs.dev.WriteAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
						return 0, fmt.Errorf("AllocateInode: failed to write bitmap for group %d: %w", groupIdx, err)
					}

					inodeNum := uint32(uint64(groupIdx)*inodesPerGroup + byteIdx*8 + bit + 1)

					bgd.BG_free_inodes_count_lo--
					if err := fs.WriteGroupDescriptor(groupIdx, bgd); err != nil {
						return 0, fmt.Errorf("AllocateInode: %w", err)
					}
					fs.updateSuperblockFreeInodes(-1)

					return inodeNum, nil
				}
			}
		}
	}

	return 0, fmt.Errorf("AllocateInode: no free inodes available")
}

func (fs *FileSystem) WriteGroupDescriptor(groupNum uint32, bgd *GroupDescriptor) error {
	sb := fs.sb
	descSize := sb.GroupDescriptorSize()

	var descTableStart uint64
	if sb.BlockSize() == 1024 {
		descTableStart = 2048
	} else {
		descTableStart = sb.BlockSize()
	}
	descOffset := int64(descTableStart) + int64(groupNum)*int64(descSize)

	var buf bytes.Buffer
	if err := binary.Write(&buf, binary.LittleEndian, bgd); err != nil {
		return fmt.Errorf("WriteGroupDescriptor: failed to encode descriptor %d: %w", groupNum, err)
	}
	raw := buf.Bytes()
	if len(raw) > int(descSize) {
		raw = raw[:descSize]
	}

	if _, err := fs.dev.WriteAt(raw, descOffset); err != nil {
		return fmt.Errorf("WriteGroupDescriptor: failed to write descriptor %d: %w", groupNum, err)
	}
	return nil
}

func InitExtentHeader(inode *Inode) {
	var hdrBuf bytes.Buffer
	binary.Write(&hdrBuf, binary.LittleEndian, ExtentHeader{
		EH_magic: EXT4_EXT_MAGIC,
		EH_entries: 0,
		EH_max:     4,
		EH_depth:   0,
	})
	copy(inode.I_block[:], hdrBuf.Bytes())
	inode.I_flags |= EXT4_EXTENTS_FL
}

func (fs *FileSystem) WriteBlock(blockNum uint64, data []byte) error {
	if uint64(len(data)) > fs.BlockSize {
		data = data[:fs.BlockSize]
	}
	_, err := fs.dev.WriteAt(data, int64(blockNum*fs.BlockSize))
	return err
}

func (fs *FileSystem) InitNewInode(inodeNum uint32, mode uint16) (*Inode, error) {
	now := uint32(time.Now().Unix())
	inode := &Inode{
		I_mode:        mode,
		I_uid:         0,
		I_gid:         0,
		I_links_count: 1,
		I_atime:       now,
		I_ctime:       now,
		I_mtime:       now,
	}
	InitExtentHeader(inode)

	if err := fs.WriteInode(inodeNum, inode); err != nil {
		return nil, fmt.Errorf("InitNewInode: %w", err)
	}
	return inode, nil
}
