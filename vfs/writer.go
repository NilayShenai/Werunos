package vfs

import (
	"fmt"
	"time"
)

func (fs *FileSystem) WriteFileAt(inode *Inode, buf []byte, ofst int64) (int, error) {
	if !inode.IsRegular() {
		return 0, fmt.Errorf("WriteFileAt: not a regular file")
	}
	if !inode.UsesExtents() {
		return 0, fmt.Errorf("WriteFileAt: extents required")
	}

	if len(buf) == 0 {
		return 0, nil
	}
	fileSize := inode.Size()
	if ofst < 0 {
		return 0, fmt.Errorf("WriteFileAt: negative offset %d", ofst)
	}
	start := uint64(ofst)
	end := start + uint64(len(buf))

	extents, err := fs.ReadExtents(inode)
	if err != nil {
		return 0, fmt.Errorf("WriteFileAt: failed to read extents: %w", err)
	}

	blockSize := fs.BlockSize
	firstBlock := start / blockSize
	lastBlock := (end - 1) / blockSize

	for logicalBlock := firstBlock; logicalBlock <= lastBlock; logicalBlock++ {
		blockStart := logicalBlock * blockSize
		blockEnd := blockStart + blockSize

		readStart := uint64(0)
		if start > blockStart {
			readStart = start - blockStart
		}
		readEnd := blockSize
		if end < blockEnd {
			readEnd = end - blockStart
		}

		physBlock, found := findPhysicalBlock(extents, uint32(logicalBlock))
		if !found {
			newPhysBlock, err := fs.AllocateBlock()
			if err != nil {
				return 0, fmt.Errorf("WriteFileAt: %w", err)
			}

			newExt := Extent{
				EE_block:    uint32(logicalBlock),
				EE_len:      1,
				EE_start_lo: uint32(newPhysBlock & 0xFFFFFFFF),
				EE_start_hi: uint16(newPhysBlock >> 32),
			}
			if err := fs.AppendExtent(inode, newExt); err != nil {
				return 0, fmt.Errorf("WriteFileAt: %w", err)
			}

			physBlock = newPhysBlock
		}

		blockData := make([]byte, blockSize)
		if _, err := fs.dev.ReadAt(blockData, int64(physBlock*blockSize)); err != nil {
			return 0, fmt.Errorf("WriteFileAt: failed to read block %d: %w", physBlock, err)
		}

		srcStart := blockStart - start
		copy(blockData[readStart:readEnd], buf[srcStart:srcStart+(readEnd-readStart)])

		if _, err := fs.dev.WriteAt(blockData, int64(physBlock*blockSize)); err != nil {
			return 0, fmt.Errorf("WriteFileAt: failed to write block %d: %w", physBlock, err)
		}
	}

	if end > fileSize {
		if inode.IsDir() {
			inode.I_size_lo = uint32(end)
		} else {
			inode.I_size_lo = uint32(end & 0xFFFFFFFF)
			inode.I_size_high = uint32(end >> 32)
		}
		newBlocks := (end + blockSize - 1) / blockSize
		sectorCount := newBlocks * (blockSize / 512)
		inode.I_blocks_lo = uint32(sectorCount & 0xFFFFFFFF)
		if sectorCount > 0xFFFFFFFF {
			inode.L_i_blocks_high = uint16(sectorCount >> 32)
		}
	}

	inode.I_mtime = uint32(time.Now().Unix())
	inode.I_ctime = inode.I_mtime

	return len(buf), nil
}

func findPhysicalBlock(extents []Extent, logicalBlock uint32) (uint64, bool) {
	for _, ext := range extents {
		if ext.EE_len&0x8000 != 0 {
			continue
		}
		blockCount := uint64(ext.EE_len & 0x7FFF)
		if logicalBlock >= ext.EE_block && logicalBlock < ext.EE_block+uint32(blockCount) {
			offset := uint64(logicalBlock - ext.EE_block)
			physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
			return physStart + offset, true
		}
	}
	return 0, false
}
