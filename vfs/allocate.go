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

	if hdr.EH_depth == 0 {
		if hdr.EH_entries < hdr.EH_max {
			return fs.appendExtentToLeaf(inode, data, &hdr, newExt)
		}
		return fs.splitRootLeafAndAppend(inode, data, &hdr, newExt)
	}

	return fs.appendExtentToTree(inode, data, &hdr, newExt)
}

func (fs *FileSystem) appendExtentToLeaf(inode *Inode, data []byte, hdr *ExtentHeader, newExt Extent) error {
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
	if err := binary.Write(&hdrBuf, binary.LittleEndian, *hdr); err != nil {
		return fmt.Errorf("AppendExtent: failed to encode header: %w", err)
	}
	copy(data[:extentHeaderSize], hdrBuf.Bytes())

	inode.I_mtime = uint32(time.Now().Unix())
	return nil
}

func (fs *FileSystem) splitRootLeafAndAppend(inode *Inode, data []byte, hdr *ExtentHeader, newExt Extent) error {
	leafBlock, err := fs.AllocateBlock()
	if err != nil {
		return fmt.Errorf("splitRootLeaf: failed to allocate leaf block: %w", err)
	}

	var extents []Extent
	for i := 0; i < int(hdr.EH_entries); i++ {
		off := extentHeaderSize + i*extentEntrySize
		var ext Extent
		if err := binary.Read(bytes.NewReader(data[off:off+extentEntrySize]), binary.LittleEndian, &ext); err != nil {
			return err
		}
		extents = append(extents, ext)
	}

	childBuf := make([]byte, fs.BlockSize)
	childHdr := ExtentHeader{
		EH_magic:   EXT4_EXT_MAGIC,
		EH_entries: uint16(len(extents)),
		EH_max:     uint16((fs.BlockSize - extentHeaderSize) / extentEntrySize),
		EH_depth:   0,
	}

	var childHdrBuf bytes.Buffer
	if err := binary.Write(&childHdrBuf, binary.LittleEndian, childHdr); err != nil {
		return err
	}
	copy(childBuf[:extentHeaderSize], childHdrBuf.Bytes())

	for i, ext := range extents {
		off := extentHeaderSize + i*extentEntrySize
		var extBuf bytes.Buffer
		if err := binary.Write(&extBuf, binary.LittleEndian, ext); err != nil {
			return err
		}
		copy(childBuf[off:], extBuf.Bytes())
	}

	childEntries := childHdr.EH_entries
	childHdr.EH_entries++

	childHdrBuf.Reset()
	_ = binary.Write(&childHdrBuf, binary.LittleEndian, childHdr)
	copy(childBuf[:extentHeaderSize], childHdrBuf.Bytes())

	entryOff := extentHeaderSize + int(childEntries)*extentEntrySize
	var newExtBuf bytes.Buffer
	_ = binary.Write(&newExtBuf, binary.LittleEndian, newExt)
	copy(childBuf[entryOff:], newExtBuf.Bytes())

	childOffset := int64(leafBlock * fs.BlockSize)
	if _, err := fs.dev.WriteAt(childBuf, childOffset); err != nil {
		return fmt.Errorf("splitRootLeaf: failed to write child leaf block: %w", err)
	}

	rootHdr := ExtentHeader{
		EH_magic:   EXT4_EXT_MAGIC,
		EH_entries: 1,
		EH_max:     4,
		EH_depth:   1,
	}

	rootIdx := ExtentIdx{
		EI_block:   extents[0].EE_block,
		EI_leaf_lo: uint32(leafBlock),
		EI_leaf_hi: uint16(leafBlock >> 32),
	}

	for i := range data {
		data[i] = 0
	}

	var rootHdrBuf bytes.Buffer
	_ = binary.Write(&rootHdrBuf, binary.LittleEndian, rootHdr)
	copy(data[:extentHeaderSize], rootHdrBuf.Bytes())

	var rootIdxBuf bytes.Buffer
	_ = binary.Write(&rootIdxBuf, binary.LittleEndian, rootIdx)
	copy(data[extentHeaderSize:], rootIdxBuf.Bytes())

	inode.I_mtime = uint32(time.Now().Unix())
	return nil
}

func (fs *FileSystem) appendExtentToTree(inode *Inode, data []byte, hdr *ExtentHeader, newExt Extent) error {
	var path []uint64
	var pathIndexes []int

	currHdr := *hdr
	currData := data
	var currBlock uint64 = 0

	for currHdr.EH_depth > 0 {
		entries := currHdr.EH_entries
		if entries == 0 {
			return fmt.Errorf("appendExtentToTree: empty index node")
		}

		lastIdx := int(entries - 1)
		off := extentHeaderSize + lastIdx*extentEntrySize
		var idx ExtentIdx
		if err := binary.Read(bytes.NewReader(currData[off:off+extentEntrySize]), binary.LittleEndian, &idx); err != nil {
			return err
		}

		childBlock := (uint64(idx.EI_leaf_hi) << 32) | uint64(idx.EI_leaf_lo)
		path = append(path, childBlock)
		pathIndexes = append(pathIndexes, lastIdx)

		childBuf := make([]byte, fs.BlockSize)
		childOffset := int64(childBlock * fs.BlockSize)
		if _, err := fs.dev.ReadAt(childBuf, childOffset); err != nil {
			return err
		}

		if err := binary.Read(bytes.NewReader(childBuf[:extentHeaderSize]), binary.LittleEndian, &currHdr); err != nil {
			return err
		}

		currData = childBuf
		currBlock = childBlock
	}

	if currHdr.EH_entries < currHdr.EH_max {
		err := fs.appendExtentToLeafBlock(currBlock, currData, &currHdr, newExt)
		if err != nil {
			return err
		}
		inode.I_mtime = uint32(time.Now().Unix())
		return nil
	}

	return fs.splitLeafBlockAndAppend(inode, path, pathIndexes, currBlock, currData, &currHdr, newExt)
}

func (fs *FileSystem) appendExtentToLeafBlock(blockNum uint64, buf []byte, hdr *ExtentHeader, newExt Extent) error {
	entries := hdr.EH_entries

	if entries > 0 {
		lastOff := extentHeaderSize + int(entries-1)*extentEntrySize
		var lastExt Extent
		if err := binary.Read(bytes.NewReader(buf[lastOff:lastOff+extentEntrySize]), binary.LittleEndian, &lastExt); err != nil {
			return err
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
					return err
				}
				copy(buf[lastOff:], encBuf.Bytes())

				childOffset := int64(blockNum * fs.BlockSize)
				_, err := fs.dev.WriteAt(buf, childOffset)
				return err
			}
		}
	}

	entryOff := extentHeaderSize + int(entries)*extentEntrySize
	var encBuf bytes.Buffer
	if err := binary.Write(&encBuf, binary.LittleEndian, newExt); err != nil {
		return err
	}
	copy(buf[entryOff:], encBuf.Bytes())

	hdr.EH_entries++
	var hdrBuf bytes.Buffer
	if err := binary.Write(&hdrBuf, binary.LittleEndian, *hdr); err != nil {
		return err
	}
	copy(buf[:extentHeaderSize], hdrBuf.Bytes())

	childOffset := int64(blockNum * fs.BlockSize)
	_, err := fs.dev.WriteAt(buf, childOffset)
	return err
}

func (fs *FileSystem) splitLeafBlockAndAppend(inode *Inode, path []uint64, pathIndexes []int, leafBlock uint64, buf []byte, hdr *ExtentHeader, newExt Extent) error {
	newLeafBlock, err := fs.AllocateBlock()
	if err != nil {
		return err
	}

	var extents []Extent
	for i := 0; i < int(hdr.EH_entries); i++ {
		off := extentHeaderSize + i*extentEntrySize
		var ext Extent
		_ = binary.Read(bytes.NewReader(buf[off:off+extentEntrySize]), binary.LittleEndian, &ext)
		extents = append(extents, ext)
	}

	insertIdx := len(extents)
	for i := 0; i < len(extents); i++ {
		if extents[i].EE_block >= newExt.EE_block {
			insertIdx = i
			break
		}
	}

	allExtents := make([]Extent, len(extents)+1)
	copy(allExtents[:insertIdx], extents[:insertIdx])
	allExtents[insertIdx] = newExt
	copy(allExtents[insertIdx+1:], extents[insertIdx:])

	mid := len(allExtents) / 2
	leftExtents := allExtents[:mid]
	rightExtents := allExtents[mid:]

	leftBuf := make([]byte, fs.BlockSize)
	leftHdr := ExtentHeader{
		EH_magic:   EXT4_EXT_MAGIC,
		EH_entries: uint16(len(leftExtents)),
		EH_max:     hdr.EH_max,
		EH_depth:   0,
	}
	var leftHdrBuf bytes.Buffer
	_ = binary.Write(&leftHdrBuf, binary.LittleEndian, leftHdr)
	copy(leftBuf[:extentHeaderSize], leftHdrBuf.Bytes())
	for i, ext := range leftExtents {
		off := extentHeaderSize + i*extentEntrySize
		var extBuf bytes.Buffer
		_ = binary.Write(&extBuf, binary.LittleEndian, ext)
		copy(leftBuf[off:], extBuf.Bytes())
	}
	if _, err := fs.dev.WriteAt(leftBuf, int64(leafBlock*fs.BlockSize)); err != nil {
		return err
	}

	rightBuf := make([]byte, fs.BlockSize)
	rightHdr := ExtentHeader{
		EH_magic:   EXT4_EXT_MAGIC,
		EH_entries: uint16(len(rightExtents)),
		EH_max:     hdr.EH_max,
		EH_depth:   0,
	}
	var rightHdrBuf bytes.Buffer
	_ = binary.Write(&rightHdrBuf, binary.LittleEndian, rightHdr)
	copy(rightBuf[:extentHeaderSize], rightHdrBuf.Bytes())
	for i, ext := range rightExtents {
		off := extentHeaderSize + i*extentEntrySize
		var extBuf bytes.Buffer
		_ = binary.Write(&extBuf, binary.LittleEndian, ext)
		copy(rightBuf[off:], extBuf.Bytes())
	}
	if _, err := fs.dev.WriteAt(rightBuf, int64(newLeafBlock*fs.BlockSize)); err != nil {
		return err
	}

	rightIdx := ExtentIdx{
		EI_block:   rightExtents[0].EE_block,
		EI_leaf_lo: uint32(newLeafBlock),
		EI_leaf_hi: uint16(newLeafBlock >> 32),
	}

	return fs.insertIdxIntoParent(inode, path, pathIndexes, rightIdx)
}

func (fs *FileSystem) insertIdxIntoParent(inode *Inode, path []uint64, pathIndexes []int, idx ExtentIdx) error {
	if len(path) == 0 {
		rootBlock, err := fs.AllocateBlock()
		if err != nil {
			return err
		}

		data := inode.I_block[:]
		var hdr ExtentHeader
		_ = binary.Read(bytes.NewReader(data[:extentHeaderSize]), binary.LittleEndian, &hdr)

		childBuf := make([]byte, fs.BlockSize)
		childHdr := ExtentHeader{
			EH_magic:   EXT4_EXT_MAGIC,
			EH_entries: hdr.EH_entries,
			EH_max:     uint16((fs.BlockSize - extentHeaderSize) / extentEntrySize),
			EH_depth:   hdr.EH_depth,
		}

		var childHdrBuf bytes.Buffer
		_ = binary.Write(&childHdrBuf, binary.LittleEndian, childHdr)
		copy(childBuf[:extentHeaderSize], childHdrBuf.Bytes())

		copy(childBuf[extentHeaderSize:], data[extentHeaderSize:extentHeaderSize+int(hdr.EH_entries)*extentEntrySize])

		childEntries := childHdr.EH_entries
		childHdr.EH_entries++

		childHdrBuf.Reset()
		_ = binary.Write(&childHdrBuf, binary.LittleEndian, childHdr)
		copy(childBuf[:extentHeaderSize], childHdrBuf.Bytes())

		var idxBuf bytes.Buffer
		_ = binary.Write(&idxBuf, binary.LittleEndian, idx)
		copy(childBuf[extentHeaderSize+int(childEntries)*extentEntrySize:], idxBuf.Bytes())

		childOffset := int64(rootBlock * fs.BlockSize)
		if _, err := fs.dev.WriteAt(childBuf, childOffset); err != nil {
			return err
		}

		rootHdr := ExtentHeader{
			EH_magic:   EXT4_EXT_MAGIC,
			EH_entries: 1,
			EH_max:     4,
			EH_depth:   hdr.EH_depth + 1,
		}

		var rootHdrBuf bytes.Buffer
		_ = binary.Write(&rootHdrBuf, binary.LittleEndian, rootHdr)
		copy(data[:extentHeaderSize], rootHdrBuf.Bytes())

		var firstIdx ExtentIdx
		_ = binary.Read(bytes.NewReader(childBuf[extentHeaderSize:extentHeaderSize+extentEntrySize]), binary.LittleEndian, &firstIdx)

		newIdx := ExtentIdx{
			EI_block:   firstIdx.EI_block,
			EI_leaf_lo: uint32(rootBlock),
			EI_leaf_hi: uint16(rootBlock >> 32),
		}
		var newIdxBuf bytes.Buffer
		_ = binary.Write(&newIdxBuf, binary.LittleEndian, newIdx)
		copy(data[extentHeaderSize:], newIdxBuf.Bytes())

		return nil
	}

	parentBlock := path[len(path)-1]

	parentBuf := make([]byte, fs.BlockSize)
	parentOffset := int64(parentBlock * fs.BlockSize)
	if _, err := fs.dev.ReadAt(parentBuf, parentOffset); err != nil {
		return err
	}

	var pHdr ExtentHeader
	_ = binary.Read(bytes.NewReader(parentBuf[:extentHeaderSize]), binary.LittleEndian, &pHdr)

	var idxs []ExtentIdx
	for i := 0; i < int(pHdr.EH_entries); i++ {
		off := extentHeaderSize + i*extentEntrySize
		var entry ExtentIdx
		_ = binary.Read(bytes.NewReader(parentBuf[off:off+extentEntrySize]), binary.LittleEndian, &entry)
		idxs = append(idxs, entry)
	}

	insertIdx := len(idxs)
	for i := 0; i < len(idxs); i++ {
		if idxs[i].EI_block >= idx.EI_block {
			insertIdx = i
			break
		}
	}

	newIdxs := make([]ExtentIdx, len(idxs)+1)
	copy(newIdxs[:insertIdx], idxs[:insertIdx])
	newIdxs[insertIdx] = idx
	copy(newIdxs[insertIdx+1:], idxs[insertIdx:])

	if len(newIdxs) <= int(pHdr.EH_max) {
		pHdr.EH_entries = uint16(len(newIdxs))
		var pHdrBuf bytes.Buffer
		_ = binary.Write(&pHdrBuf, binary.LittleEndian, pHdr)
		copy(parentBuf[:extentHeaderSize], pHdrBuf.Bytes())

		for i, entry := range newIdxs {
			off := extentHeaderSize + i*extentEntrySize
			var entryBuf bytes.Buffer
			_ = binary.Write(&entryBuf, binary.LittleEndian, entry)
			copy(parentBuf[off:], entryBuf.Bytes())
		}

		_, err := fs.dev.WriteAt(parentBuf, parentOffset)
		return err
	}

	newParentBlock, err := fs.AllocateBlock()
	if err != nil {
		return err
	}

	mid := len(newIdxs) / 2
	leftIdxs := newIdxs[:mid]
	rightIdxs := newIdxs[mid:]

	leftBuf := make([]byte, fs.BlockSize)
	leftHdr := ExtentHeader{
		EH_magic:   EXT4_EXT_MAGIC,
		EH_entries: uint16(len(leftIdxs)),
		EH_max:     pHdr.EH_max,
		EH_depth:   pHdr.EH_depth,
	}
	var leftHdrBuf bytes.Buffer
	_ = binary.Write(&leftHdrBuf, binary.LittleEndian, leftHdr)
	copy(leftBuf[:extentHeaderSize], leftHdrBuf.Bytes())
	for i, entry := range leftIdxs {
		off := extentHeaderSize + i*extentEntrySize
		var entryBuf bytes.Buffer
		_ = binary.Write(&entryBuf, binary.LittleEndian, entry)
		copy(leftBuf[off:], entryBuf.Bytes())
	}
	if _, err := fs.dev.WriteAt(leftBuf, int64(parentBlock*fs.BlockSize)); err != nil {
		return err
	}

	rightBuf := make([]byte, fs.BlockSize)
	rightHdr := ExtentHeader{
		EH_magic:   EXT4_EXT_MAGIC,
		EH_entries: uint16(len(rightIdxs)),
		EH_max:     pHdr.EH_max,
		EH_depth:   pHdr.EH_depth,
	}
	var rightHdrBuf bytes.Buffer
	_ = binary.Write(&rightHdrBuf, binary.LittleEndian, rightHdr)
	copy(rightBuf[:extentHeaderSize], rightHdrBuf.Bytes())
	for i, entry := range rightIdxs {
		off := extentHeaderSize + i*extentEntrySize
		var entryBuf bytes.Buffer
		_ = binary.Write(&entryBuf, binary.LittleEndian, entry)
		copy(rightBuf[off:], entryBuf.Bytes())
	}
	if _, err := fs.dev.WriteAt(rightBuf, int64(newParentBlock*fs.BlockSize)); err != nil {
		return err
	}

	parentIdx := ExtentIdx{
		EI_block:   rightIdxs[0].EI_block,
		EI_leaf_lo: uint32(newParentBlock),
		EI_leaf_hi: uint16(newParentBlock >> 32),
	}
	return fs.insertIdxIntoParent(inode, path[:len(path)-1], pathIndexes[:len(pathIndexes)-1], parentIdx)
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
