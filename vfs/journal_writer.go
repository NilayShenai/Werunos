package vfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"
)

type Tx struct {
	fs     *FileSystem
	dirty  map[uint64][]byte // block number -> 4KB block data
	active bool
	mu     sync.Mutex
}

type TxDevice struct {
	inner ReadWriterAt
	tx    *Tx
}

func NewTxDevice(inner ReadWriterAt, tx *Tx) *TxDevice {
	return &TxDevice{
		inner: inner,
		tx:    tx,
	}
}

func (td *TxDevice) ReadAt(p []byte, off int64) (int, error) {
	n, err := td.inner.ReadAt(p, off)
	if err != nil && err != io.EOF {
		return n, err
	}

	td.tx.mu.Lock()
	defer td.tx.mu.Unlock()

	blockSize := int64(td.tx.fs.BlockSize)
	readEnd := off + int64(n)
	for blockNum, blockData := range td.tx.dirty {
		blockStart := int64(blockNum) * blockSize
		blockEnd := blockStart + blockSize

		overlapStart := maxVal(off, blockStart)
		overlapEnd := minVal(readEnd, blockEnd)
		if overlapStart < overlapEnd {
			pStart := overlapStart - off
			bStart := overlapStart - blockStart
			copy(p[pStart:pStart+(overlapEnd-overlapStart)], blockData[bStart:bStart+(overlapEnd-overlapStart)])
		}
	}
	return n, err
}

func (td *TxDevice) WriteAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	td.tx.mu.Lock()
	defer td.tx.mu.Unlock()

	blockSize := int64(td.tx.fs.BlockSize)
	writeEnd := off + int64(len(p))

	startBlock := uint64(off / blockSize)
	endBlock := uint64((writeEnd - 1) / blockSize)

	for blockNum := startBlock; blockNum <= endBlock; blockNum++ {
		blockStart := int64(blockNum) * blockSize
		blockEnd := blockStart + blockSize

		blockData, exists := td.tx.dirty[blockNum]
		if !exists {
			blockData = make([]byte, blockSize)
			_, err := td.inner.ReadAt(blockData, blockStart)
			if err != nil && err != io.EOF {
				return 0, err
			}
			td.tx.dirty[blockNum] = blockData
		}

		overlapStart := maxVal(off, blockStart)
		overlapEnd := minVal(writeEnd, blockEnd)
		pStart := overlapStart - off
		bStart := overlapStart - blockStart
		copy(blockData[bStart:bStart+(overlapEnd-overlapStart)], p[pStart:pStart+(overlapEnd-overlapStart)])
	}

	return len(p), nil
}

func maxVal(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minVal(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (fs *FileSystem) BeginTransaction() {
	fs.txMu.Lock()
	defer fs.txMu.Unlock()

	if fs.dev == nil {
		return
	}
	// Check if already in transaction
	if _, ok := fs.dev.(*TxDevice); ok {
		return
	}

	tx := &Tx{
		fs:    fs,
		dirty: make(map[uint64][]byte),
	}
	fs.dev = NewTxDevice(fs.dev, tx)
}

func (fs *FileSystem) CommitTransaction() error {
	fs.txMu.Lock()
	defer fs.txMu.Unlock()

	td, ok := fs.dev.(*TxDevice)
	if !ok {
		return nil
	}

	tx := td.tx
	tx.mu.Lock()
	defer tx.mu.Unlock()

	// Restore original device
	fs.dev = td.inner

	if len(tx.dirty) == 0 {
		return nil
	}

	// 1. Locate journal
	journalInum := fs.sb.S_journal_inum
	if journalInum == 0 {
		// Fallback: write dirty blocks directly if no journal
		return fs.writeDirtyBlocksDirectly(tx.dirty)
	}

	jinode, err := fs.ReadInode(journalInum)
	if err != nil {
		return fmt.Errorf("jbd2 commit: read journal inode: %w", err)
	}

	extents, err := fs.ReadExtents(jinode)
	if err != nil {
		return fmt.Errorf("jbd2 commit: read journal extents: %w", err)
	}

	// 2. Read JBD2 superblock
	jsbBlock, found := findPhysicalBlock(extents, 0)
	if !found {
		return fmt.Errorf("jbd2 commit: journal superblock block not found")
	}

	blockSize := int(fs.BlockSize)
	jsbBuf := make([]byte, blockSize)
	if _, err := fs.dev.ReadAt(jsbBuf, int64(jsbBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("jbd2 commit: read journal superblock: %w", err)
	}

	var jsb jbd2Superblock
	jsb.Header.H_magic = binary.LittleEndian.Uint32(jsbBuf[0:4])
	jsb.Header.H_blocktype = binary.LittleEndian.Uint32(jsbBuf[4:8])
	jsb.Header.H_sequence = binary.LittleEndian.Uint32(jsbBuf[8:12])
	jsb.S_sequence = binary.LittleEndian.Uint32(jsbBuf[12:16])
	jsb.S_start = binary.LittleEndian.Uint32(jsbBuf[16:20])
	jsb.S_maxlen = binary.LittleEndian.Uint32(jsbBuf[20:24])

	if jsb.Header.H_magic != JBD2_MAGIC && jsb.Header.H_magic != JBD2_MAGIC_OLD {
		// Fallback to direct writes if journal is uninitialized
		return fs.writeDirtyBlocksDirectly(tx.dirty)
	}

	// Calculate circular position
	pos := jsb.S_start
	if pos == 0 || pos > jsb.S_maxlen {
		pos = 1
	}

	sequence := jsb.S_sequence

	// 3. Write Descriptor Block
	descBuf := make([]byte, blockSize)
	binary.LittleEndian.PutUint32(descBuf[0:4], JBD2_MAGIC)
	binary.LittleEndian.PutUint32(descBuf[4:8], JBD2_DESCRIPTOR)
	binary.LittleEndian.PutUint32(descBuf[8:12], sequence)

	off := 12
	tagSize := 8

	var dataBlocksToWrite [][]byte
	var dataFlags []uint32

	blockIndex := 0
	for blockNum, blockData := range tx.dirty {
		escaped := false
		if len(blockData) >= 4 && binary.LittleEndian.Uint32(blockData[0:4]) == JBD2_MAGIC {
			escaped = true
		}

		dataCopy := make([]byte, blockSize)
		copy(dataCopy, blockData)
		if escaped {
			escBytes := [4]byte{0x4A, 0x6F, 0x6E, 0x75}
			for i := 0; i < 4; i++ {
				dataCopy[i] ^= escBytes[i]
			}
		}

		dataBlocksToWrite = append(dataBlocksToWrite, dataCopy)

		flags := uint32(0)
		if escaped {
			flags |= JBD2_FLAG_ESCAPE
		}
		if blockIndex == len(tx.dirty)-1 {
			flags |= JBD2_FLAG_LAST_TAG
		}
		dataFlags = append(dataFlags, flags)

		// Put tag
		binary.LittleEndian.PutUint32(descBuf[off:off+4], uint32(blockNum))
		binary.LittleEndian.PutUint32(descBuf[off+4:off+8], flags)
		off += tagSize
		blockIndex++
	}

	// Write descriptor block
	descBlockPhys, found := findPhysicalBlock(extents, pos)
	if !found {
		return fmt.Errorf("jbd2 commit: physical block for descriptor pos %d not found", pos)
	}
	if _, err := fs.dev.WriteAt(descBuf, int64(descBlockPhys*fs.BlockSize)); err != nil {
		return fmt.Errorf("jbd2 commit: write descriptor block: %w", err)
	}

	// 4. Write Data Blocks
	for i, dataBuf := range dataBlocksToWrite {
		pos++
		if pos > jsb.S_maxlen {
			pos = 1
		}
		dataBlockPhys, found := findPhysicalBlock(extents, pos)
		if !found {
			return fmt.Errorf("jbd2 commit: physical block for data pos %d not found", pos)
		}
		if _, err := fs.dev.WriteAt(dataBuf, int64(dataBlockPhys*fs.BlockSize)); err != nil {
			return fmt.Errorf("jbd2 commit: write data block: %w", err)
		}
		_ = dataFlags[i]
	}

	// 5. Write Commit Block
	pos++
	if pos > jsb.S_maxlen {
		pos = 1
	}
	commitBuf := make([]byte, blockSize)
	binary.LittleEndian.PutUint32(commitBuf[0:4], JBD2_MAGIC)
	binary.LittleEndian.PutUint32(commitBuf[4:8], JBD2_COMMIT)
	binary.LittleEndian.PutUint32(commitBuf[8:12], sequence)

	// Add commit time
	now := time.Now()
	binary.LittleEndian.PutUint64(commitBuf[16:24], uint64(now.Unix()))

	commitBlockPhys, found := findPhysicalBlock(extents, pos)
	if !found {
		return fmt.Errorf("jbd2 commit: physical block for commit pos %d not found", pos)
	}
	if _, err := fs.dev.WriteAt(commitBuf, int64(commitBlockPhys*fs.BlockSize)); err != nil {
		return fmt.Errorf("jbd2 commit: write commit block: %w", err)
	}

	// 6. Update JBD2 Superblock
	nextStart := pos + 1
	if nextStart > jsb.S_maxlen {
		nextStart = 1
	}

	binary.LittleEndian.PutUint32(jsbBuf[12:16], sequence+1)
	binary.LittleEndian.PutUint32(jsbBuf[16:20], nextStart)

	if _, err := fs.dev.WriteAt(jsbBuf, int64(jsbBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("jbd2 commit: write journal superblock updates: %w", err)
	}

	// 7. Write the dirty blocks back to their final physical locations on disk
	if err := fs.writeDirtyBlocksDirectly(tx.dirty); err != nil {
		return fmt.Errorf("jbd2 commit: final block writeback failed: %w", err)
	}

	return nil
}

func (fs *FileSystem) writeDirtyBlocksDirectly(dirty map[uint64][]byte) error {
	for blockNum, blockData := range dirty {
		offset := int64(blockNum * fs.BlockSize)
		if _, err := fs.dev.WriteAt(blockData, offset); err != nil {
			return err
		}
	}
	return nil
}

func (fs *FileSystem) RollbackTransaction() {
	fs.txMu.Lock()
	defer fs.txMu.Unlock()

	td, ok := fs.dev.(*TxDevice)
	if !ok {
		return
	}
	fs.dev = td.inner
}
