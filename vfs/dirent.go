package vfs

import (
	"encoding/binary"
	"fmt"
	"time"
)

func minDirRecLen(nameLen int) uint16 {
	n := dirEntryHeaderSize + nameLen
	return uint16(((n + 3) / 4) * 4)
}

func writeDirBlock(entries []DirEntry2, blockSize int) []byte {
	buf := make([]byte, blockSize)
	offset := 0
	for i, e := range entries {
		nameLen := len(e.Name)
		recLen := minDirRecLen(nameLen)
		if i == len(entries)-1 {
			recLen = uint16(blockSize - offset)
		}
		binary.LittleEndian.PutUint32(buf[offset:offset+4], e.Inode)
		binary.LittleEndian.PutUint16(buf[offset+4:offset+6], recLen)
		buf[offset+6] = uint8(nameLen)
		buf[offset+7] = e.FileType
		copy(buf[offset+dirEntryHeaderSize:offset+dirEntryHeaderSize+nameLen], e.Name)
		offset += int(recLen)
	}
	return buf
}

func (fs *FileSystem) AddDirEntry(dirInode *Inode, name string, childInodeNum uint32, fileType uint8) error {
	if !dirInode.IsDir() {
		return fmt.Errorf("AddDirEntry: inode is not a directory")
	}

	if _, err := fs.Lookup(dirInode, name); err == nil {
		return fmt.Errorf("AddDirEntry: entry %q already exists", name)
	}

	extents, err := fs.ReadExtents(dirInode)
	if err != nil {
		return fmt.Errorf("AddDirEntry: failed to read extents: %w", err)
	}

	blockSize := int(fs.BlockSize)
	needed := minDirRecLen(len(name))

	for _, ext := range extents {
		blockCount := uint64(ext.EE_len & 0x7FFF)
		physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)

		for i := uint64(0); i < blockCount; i++ {
			physBlock := physStart + i
			blockOffset := int64(physBlock * fs.BlockSize)

			buf := make([]byte, blockSize)
			if _, err := fs.dev.ReadAt(buf, blockOffset); err != nil {
				return fmt.Errorf("AddDirEntry: failed to read block %d: %w", physBlock, err)
			}

			offset := 0
			for offset < blockSize {
				if offset+dirEntryHeaderSize > blockSize {
					break
				}
				recLen := binary.LittleEndian.Uint16(buf[offset+4 : offset+6])
				if recLen == 0 {
					break
				}

				nameLen := buf[offset+6]
				thisMin := minDirRecLen(int(nameLen))

				if uint16(offset)+recLen == uint16(blockSize) {

					slack := int(recLen) - int(thisMin)
					if slack >= int(needed) {

						binary.LittleEndian.PutUint16(buf[offset+4:offset+6], thisMin)

						newOff := offset + int(thisMin)
						newRecLen := uint16(blockSize - newOff)
						binary.LittleEndian.PutUint32(buf[newOff:newOff+4], childInodeNum)
						binary.LittleEndian.PutUint16(buf[newOff+4:newOff+6], newRecLen)
						buf[newOff+6] = uint8(len(name))
						buf[newOff+7] = fileType
						copy(buf[newOff+dirEntryHeaderSize:], name)

						if _, err := fs.dev.WriteAt(buf, blockOffset); err != nil {
							return fmt.Errorf("AddDirEntry: failed to write block %d: %w", physBlock, err)
						}
						fs.invalidateDirCache(dirInode)
						dirInode.I_mtime = uint32(time.Now().Unix())
						return nil
					}
				} else {

					slack := int(recLen) - int(thisMin)
					if slack >= int(needed) {

						binary.LittleEndian.PutUint16(buf[offset+4:offset+6], thisMin)

						newOff := offset + int(thisMin)
						remaining := int(recLen) - int(thisMin)
						newRecLen := uint16(remaining)
						binary.LittleEndian.PutUint32(buf[newOff:newOff+4], childInodeNum)
						binary.LittleEndian.PutUint16(buf[newOff+4:newOff+6], newRecLen)
						buf[newOff+6] = uint8(len(name))
						buf[newOff+7] = fileType
						copy(buf[newOff+dirEntryHeaderSize:], name)

						if _, err := fs.dev.WriteAt(buf, blockOffset); err != nil {
							return fmt.Errorf("AddDirEntry: failed to write block %d: %w", physBlock, err)
						}
						fs.invalidateDirCache(dirInode)
						dirInode.I_mtime = uint32(time.Now().Unix())
						return nil
					}
				}

				offset += int(recLen)
			}
		}
	}

	newBlock, err := fs.AllocateBlock()
	if err != nil {
		return fmt.Errorf("AddDirEntry: failed to allocate block: %w", err)
	}

	newExt := Extent{
		EE_block:    uint32(dirInode.Size() / fs.BlockSize),
		EE_len:      1,
		EE_start_lo: uint32(newBlock & 0xFFFFFFFF),
		EE_start_hi: uint16(newBlock >> 32),
	}
	if err := fs.AppendExtent(dirInode, newExt); err != nil {
		return fmt.Errorf("AddDirEntry: %w", err)
	}

	entry := DirEntry2{
		Inode:    childInodeNum,
		NameLen:  uint8(len(name)),
		FileType: fileType,
		Name:     name,
	}
	blockBuf := writeDirBlock([]DirEntry2{entry}, blockSize)
	if _, err := fs.dev.WriteAt(blockBuf, int64(newBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("AddDirEntry: failed to write new block %d: %w", newBlock, err)
	}

	newSize := dirInode.Size() + uint64(blockSize)
	if dirInode.IsDir() {
		dirInode.I_size_lo = uint32(newSize)
	} else {
		dirInode.I_size_lo = uint32(newSize & 0xFFFFFFFF)
		dirInode.I_size_high = uint32(newSize >> 32)
	}

	fs.invalidateDirCache(dirInode)
	dirInode.I_mtime = uint32(time.Now().Unix())
	return nil
}

func (fs *FileSystem) RemoveDirEntry(dirInode *Inode, name string) error {
	if !dirInode.IsDir() {
		return fmt.Errorf("RemoveDirEntry: inode is not a directory")
	}

	extents, err := fs.ReadExtents(dirInode)
	if err != nil {
		return fmt.Errorf("RemoveDirEntry: failed to read extents: %w", err)
	}

	blockSize := int(fs.BlockSize)

	for _, ext := range extents {
		blockCount := uint64(ext.EE_len & 0x7FFF)
		physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)

		for i := uint64(0); i < blockCount; i++ {
			physBlock := physStart + i
			blockOffset := int64(physBlock * fs.BlockSize)

			buf := make([]byte, blockSize)
			if _, err := fs.dev.ReadAt(buf, blockOffset); err != nil {
				return fmt.Errorf("RemoveDirEntry: failed to read block %d: %w", physBlock, err)
			}

			offset := 0
			var prevOff int
			for offset < blockSize {
				if offset+dirEntryHeaderSize > blockSize {
					break
				}
				inode := binary.LittleEndian.Uint32(buf[offset : offset+4])
				recLen := binary.LittleEndian.Uint16(buf[offset+4 : offset+6])
				if recLen == 0 {
					break
				}
				nameLen := buf[offset+6]
				if inode != 0 && nameLen > 0 {
					nameStart := offset + dirEntryHeaderSize
					nameEnd := nameStart + int(nameLen)
					if nameEnd <= blockSize && string(buf[nameStart:nameEnd]) == name {

						binary.LittleEndian.PutUint32(buf[offset:offset+4], 0)

						if prevOff > 0 {
							newPrevRecLen := uint16((offset + int(recLen)) - prevOff)
							binary.LittleEndian.PutUint16(buf[prevOff+4:prevOff+6], newPrevRecLen)
						}

						if _, err := fs.dev.WriteAt(buf, blockOffset); err != nil {
							return fmt.Errorf("RemoveDirEntry: failed to write block %d: %w", physBlock, err)
						}
						fs.invalidateDirCache(dirInode)
						dirInode.I_mtime = uint32(time.Now().Unix())
						return nil
					}
				}
				prevOff = offset
				offset += int(recLen)
			}
		}
	}

	return ErrNotExist
}

func (fs *FileSystem) invalidateDirCache(inode *Inode) {
	fs.dirCache.Delete(inode.I_block)
}
