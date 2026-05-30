package vfs

import (
	"encoding/binary"
	"fmt"
)

type DirEntry2 struct {

	Inode uint32

	RecLen uint16

	NameLen uint8

	FileType uint8

	Name string
}

const (
	DirFileTypeUnknown  = 0x0
	DirFileTypeRegular  = 0x1
	DirFileTypeDir      = 0x2
	DirFileTypeCharDev  = 0x3
	DirFileTypeBlockDev = 0x4
	DirFileTypeFIFO     = 0x5
	DirFileTypeSocket   = 0x6
	DirFileTypeSymlink  = 0x7
)

var DirFileTypeName = map[uint8]string{
	DirFileTypeUnknown:  "?",
	DirFileTypeRegular:  "f",
	DirFileTypeDir:      "d",
	DirFileTypeCharDev:  "c",
	DirFileTypeBlockDev: "b",
	DirFileTypeFIFO:     "p",
	DirFileTypeSocket:   "s",
	DirFileTypeSymlink:  "l",
}

const dirEntryHeaderSize = 8

func (fs *FileSystem) ReadDir(inode *Inode) ([]DirEntry2, error) {

	if !inode.IsDir() {
		return nil, fmt.Errorf(
			"ReadDir called on a non-directory inode (I_mode=0x%04x)",
			inode.I_mode,
		)
	}

	isHtree := inode.I_flags&EXT4_INDEX_FL != 0

	extents, err := fs.ReadExtents(inode)
	if err != nil {
		return nil, fmt.Errorf("failed to read extent tree: %w", err)
	}

	var entries []DirEntry2

	logicalBlock := uint64(0)

	for _, ext := range extents {

		physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)

		for i := range uint64(ext.EE_len) {
			physBlock := physStart + i
			blockOffset := int64(physBlock * fs.BlockSize)

			buf := make([]byte, fs.BlockSize)
			_, err := fs.dev.ReadAt(buf, blockOffset)
			if err != nil {
				return nil, fmt.Errorf(
					"failed to read directory block %d (offset %d): %w",
					physBlock, blockOffset, err,
				)
			}

			var (
				blockEntries []DirEntry2
				parseErr     error
			)
			if isHtree && logicalBlock == 0 {

				blockEntries, parseErr = parseDirBlockHtreeRoot(buf)
			} else {

				blockEntries, parseErr = parseDirBlock(buf)
			}
			if parseErr != nil {
				return nil, fmt.Errorf("failed to parse directory block %d: %w", physBlock, parseErr)
			}
			entries = append(entries, blockEntries...)
			logicalBlock++
		}
	}

	return entries, nil
}

func (fs *FileSystem) ReadDirCached(inode *Inode) ([]DirEntry2, error) {
	return fs.ReadDir(inode)
}

func (fs *FileSystem) Lookup(dirInode *Inode, name string) (uint32, error) {
	entries, err := fs.ReadDirCached(dirInode)
	if err != nil {
		return 0, fmt.Errorf("Lookup(%q): failed to read directory: %w", name, err)
	}
	for _, e := range entries {
		if e.Name == name {
			return e.Inode, nil
		}
	}
	return 0, ErrNotExist
}

func parseDirBlockHtreeRoot(buf []byte) ([]DirEntry2, error) {
	var entries []DirEntry2
	offset := 0
	blockSize := len(buf)

	for range 2 {
		if offset+dirEntryHeaderSize > blockSize {
			break
		}

		inode := binary.LittleEndian.Uint32(buf[offset : offset+4])
		recLen := binary.LittleEndian.Uint16(buf[offset+4 : offset+6])
		nameLen := buf[offset+6]
		fileType := buf[offset+7]

		if recLen == 0 {
			return nil, fmt.Errorf("htree root: entry at offset %d has rec_len=0 (corrupt block)", offset)
		}

		if inode != 0 && nameLen > 0 {
			nameStart := offset + dirEntryHeaderSize
			nameEnd := nameStart + int(nameLen)
			if nameEnd > blockSize {
				return nil, fmt.Errorf(
					"htree root: entry at offset %d: name extends beyond block boundary (%d > %d)",
					offset, nameEnd, blockSize,
				)
			}
			entries = append(entries, DirEntry2{
				Inode:    inode,
				RecLen:   recLen,
				NameLen:  nameLen,
				FileType: fileType,
				Name:     string(buf[nameStart:nameEnd]),
			})
		}

		offset += int(recLen)
	}

	return entries, nil
}

func parseDirBlock(buf []byte) ([]DirEntry2, error) {
	var entries []DirEntry2
	offset := 0
	blockSize := len(buf)

	for offset < blockSize {

		if offset+dirEntryHeaderSize > blockSize {
			break
		}

		inode := binary.LittleEndian.Uint32(buf[offset : offset+4])
		recLen := binary.LittleEndian.Uint16(buf[offset+4 : offset+6])
		nameLen := buf[offset+6]
		fileType := buf[offset+7]

		if recLen == 0 {
			return nil, fmt.Errorf("directory entry at offset %d has rec_len=0 (corrupt block)", offset)
		}

		if inode != 0 && nameLen > 0 {
			nameStart := offset + dirEntryHeaderSize
			nameEnd := nameStart + int(nameLen)
			if nameEnd > blockSize {
				return nil, fmt.Errorf(
					"directory entry at offset %d: name extends beyond block boundary (%d > %d)",
					offset, nameEnd, blockSize,
				)
			}
			name := string(buf[nameStart:nameEnd])

			entries = append(entries, DirEntry2{
				Inode:    inode,
				RecLen:   recLen,
				NameLen:  nameLen,
				FileType: fileType,
				Name:     name,
			})
		}

		offset += int(recLen)
	}

	return entries, nil
}
