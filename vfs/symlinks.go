package vfs

import (
	"fmt"
)

func (fs *FileSystem) ReadSymlink(inode *Inode) (string, error) {

	if !inode.IsSymlink() {
		return "", fmt.Errorf(
			"ReadSymlink: inode has I_mode=0x%04x - expected S_IFLNK (0xA000)",
			inode.I_mode,
		)
	}

	size := inode.Size()
	if size == 0 {

		return "", nil
	}

	if !inode.UsesExtents() && size <= 60 {

		return string(inode.I_block[:size]), nil
	}

	extents, err := fs.ReadExtents(inode)
	if err != nil {
		return "", fmt.Errorf("ReadSymlink: failed to walk extent tree: %w", err)
	}

	buf := make([]byte, size)
	written := uint64(0)

	for _, ext := range extents {
		if written >= size {
			break
		}

		if ext.EE_len&0x8000 != 0 {
			blockCount := uint64(ext.EE_len & 0x7FFF)
			written += blockCount * fs.BlockSize
			continue
		}

		physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
		blockCount := uint64(ext.EE_len)

		for i := uint64(0); i < blockCount; i++ {
			if written >= size {
				break
			}
			physBlock := physStart + i
			blockBuf := make([]byte, fs.BlockSize)
			if _, err := fs.dev.ReadAt(blockBuf, int64(physBlock*fs.BlockSize)); err != nil {
				return "", fmt.Errorf("ReadSymlink: failed to read block %d: %w", physBlock, err)
			}
			remaining := size - written
			toCopy := fs.BlockSize
			if toCopy > remaining {
				toCopy = remaining
			}
			copy(buf[written:written+toCopy], blockBuf[:toCopy])
			written += toCopy
		}
	}

	return string(buf), nil
}
