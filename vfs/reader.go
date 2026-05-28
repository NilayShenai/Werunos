package vfs

import (
	"fmt"
)

func (fs *FileSystem) ReadFile(inode *Inode) ([]byte, error) {

	if !inode.IsRegular() {
		return nil, fmt.Errorf(
			"ReadFile: inode has I_mode=0x%04x - only regular files (S_IFREG) are supported",
			inode.I_mode,
		)
	}

	if !inode.UsesExtents() {
		return nil, fmt.Errorf(
			"ReadFile: inode uses legacy indirect block addressing (EXT4_EXTENTS_FL not set, I_flags=0x%08x); "+
				"indirect block scheme is not yet supported",
			inode.I_flags,
		)
	}

	extents, err := fs.ReadExtents(inode)
	if err != nil {
		return nil, fmt.Errorf("ReadFile: failed to walk extent tree: %w", err)
	}

	fileSize := inode.Size()
	if fileSize == 0 {
		return []byte{}, nil
	}

	out := make([]byte, fileSize)
	written := uint64(0)

	for _, ext := range extents {
		if written >= fileSize {

			break
		}

		uninitialized := ext.EE_len&0x8000 != 0
		blockCount := uint64(ext.EE_len & 0x7FFF)

		if uninitialized {

			zeroes := blockCount * fs.BlockSize
			if written+zeroes > fileSize {
				zeroes = fileSize - written
			}
			written += zeroes
			continue
		}

		physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)

		for i := uint64(0); i < blockCount; i++ {
			if written >= fileSize {
				break
			}

			physBlock := physStart + i
			blockOffset := int64(physBlock * fs.BlockSize)

			blockBuf := make([]byte, fs.BlockSize)
			if _, err := fs.dev.ReadAt(blockBuf, blockOffset); err != nil {
				return nil, fmt.Errorf(
					"ReadFile: failed to read block %d at byte offset %d: %w",
					physBlock, blockOffset, err,
				)
			}

			remaining := fileSize - written
			toCopy := fs.BlockSize
			if toCopy > remaining {
				toCopy = remaining
			}
			copy(out[written:written+toCopy], blockBuf[:toCopy])
			written += toCopy
		}
	}

	return out, nil
}

func (fs *FileSystem) ReadFileAt(extents []Extent, fileSize uint64, buf []byte, ofst int64) (int, error) {
	if ofst < 0 || uint64(ofst) >= fileSize {
		return 0, nil
	}

	start := uint64(ofst)
	end := start + uint64(len(buf))
	if end > fileSize {
		end = fileSize
	}
	totalToRead := int(end - start)

	for i := range buf[:totalToRead] {
		buf[i] = 0
	}

	logicalPos := uint64(0)

	for _, ext := range extents {
		uninitialized := ext.EE_len&0x8000 != 0
		blockCount := uint64(ext.EE_len & 0x7FFF)
		extentBytes := blockCount * fs.BlockSize
		extentEnd := logicalPos + extentBytes

		if extentEnd <= start {
			logicalPos = extentEnd
			continue
		}

		if logicalPos >= end {
			break
		}

		if uninitialized {

			logicalPos = extentEnd
			continue
		}

		physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)

		for i := uint64(0); i < blockCount; i++ {
			blockStart := logicalPos + i*fs.BlockSize
			blockEnd := blockStart + fs.BlockSize

			if blockEnd <= start {
				continue
			}
			if blockStart >= end {
				break
			}

			physBlock := physStart + i
			blockBuf := make([]byte, fs.BlockSize)
			if _, err := fs.dev.ReadAt(blockBuf, int64(physBlock*fs.BlockSize)); err != nil {
				return 0, fmt.Errorf("ReadFileAt: block %d: %w", physBlock, err)
			}

			srcStart := uint64(0)
			if start > blockStart {
				srcStart = start - blockStart
			}
			srcEnd := fs.BlockSize
			if end < blockEnd {
				srcEnd = end - blockStart
			}

			dstStart := blockStart + srcStart - start
			copy(buf[dstStart:], blockBuf[srcStart:srcEnd])
		}

		logicalPos = extentEnd
	}

	return totalToRead, nil
}
