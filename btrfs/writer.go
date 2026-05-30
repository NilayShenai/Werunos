package btrfs

import (
	"encoding/binary"
	"fmt"
	"sort"
	"time"
)

type itemEntry struct {
	k    key
	data []byte
}

func (b *FileSystem) insertIntoLeaf(logicalAddr uint64, k key, itemData []byte) error {
	leafLogical, err := b.resolveLeaf(logicalAddr, k)
	if err != nil {
		return err
	}
	physAddr, err := b.tc.resolve(leafLogical)
	if err != nil {
		return err
	}
	bs := int(b.sb.NodeSize)
	buf := make([]byte, bs)
	if _, err := b.dev.ReadAt(buf, int64(physAddr)); err != nil {
		return fmt.Errorf("btrfs: read leaf: %w", err)
	}

	nritems := int(binary.LittleEndian.Uint32(buf[96:100]))

	// Parse existing items: read key + offset + size from each item record
	type itemRec struct {
		key    key
		offset uint32
		size   uint32
		data   []byte
	}
	items := make([]itemRec, nritems)
	for i := 0; i < nritems; i++ {
		off := nodeHeaderSize + i*25
		if off+25 > bs {
			items = items[:i]
			nritems = i
			break
		}
		items[i] = itemRec{
			key: key{
				objectid: binary.LittleEndian.Uint64(buf[off:]),
				typ:      buf[off+8],
				offset:   binary.LittleEndian.Uint64(buf[off+9:]),
			},
			offset: binary.LittleEndian.Uint32(buf[off+17:]),
			size:   binary.LittleEndian.Uint32(buf[off+21:]),
		}
		d := make([]byte, items[i].size)
		di := nodeHeaderSize + int(items[i].offset)
		if di < 0 || di+int(items[i].size) > bs {
			return fmt.Errorf("btrfs: item %d data at %d+%d out of bounds", i, di, items[i].size)
		}
		copy(d, buf[di:di+int(items[i].size)])
		items[i].data = d
	}

	insertIdx := nritems
	for i := 0; i < nritems; i++ {
		if !items[i].key.less(k) {
			insertIdx = i
			break
		}
	}

	if nritems >= 0xFFF0 {
		return fmt.Errorf("btrfs: leaf full")
	}

	all := make([]itemEntry, nritems+1)
	ins := insertIdx
	for i := 0; i < ins; i++ {
		all[i] = itemEntry{k: items[i].key, data: items[i].data}
	}
	all[ins] = itemEntry{k: k, data: itemData}
	for i := ins; i < nritems; i++ {
		all[i+1] = itemEntry{k: items[i].key, data: items[i].data}
	}

	// Calculate total data size
	totalData := 0
	for _, it := range all {
		totalData += len(it.data)
	}

	// Check space: header + items + data must fit in node
	needed := nodeHeaderSize + len(all)*25 + totalData
	if needed > bs {
		return b.splitLeaf(leafLogical, k, all, bs, ins)
	}

	// Build new leaf
	dst := make([]byte, bs)
	copy(dst[:nodeHeaderSize], buf[:nodeHeaderSize])

	// Write data area: fill from the end going backward
	dataPos := bs
	for i := len(all) - 1; i >= 0; i-- {
		sz := len(all[i].data)
		dataPos -= sz
		copy(dst[dataPos:dataPos+sz], all[i].data)
	}

	// Write item records
	curDataOff := dataPos
	for i, it := range all {
		off := nodeHeaderSize + i*25
		encodeKeyInto(dst[off:off+17], it.k)
		relOff := curDataOff - nodeHeaderSize
		binary.LittleEndian.PutUint32(dst[off+17:off+21], uint32(relOff))
		binary.LittleEndian.PutUint32(dst[off+21:off+25], uint32(len(it.data)))
		curDataOff += len(it.data)
	}

	binary.LittleEndian.PutUint32(dst[96:100], uint32(len(all)))
	binary.LittleEndian.PutUint64(dst[80:88], b.sb.Generation)

	binary.LittleEndian.PutUint64(dst[48:56], uint64(leafLogical))

	csum := calcCrc32c(dst[32:])
	binary.LittleEndian.PutUint32(dst[0:4], csum)

	if _, err := b.dev.WriteAt(dst, int64(physAddr)); err != nil {
		return fmt.Errorf("btrfs: write leaf: %w", err)
	}
	return nil
}

func encodeKeyInto(dst []byte, k key) {
	binary.LittleEndian.PutUint64(dst[0:8], k.objectid)
	dst[8] = k.typ
	binary.LittleEndian.PutUint64(dst[9:17], k.offset)
}

func encodeKey(k key) []byte {
	buf := make([]byte, 17)
	encodeKeyInto(buf, k)
	return buf
}

func makeDirItem(childInode uint64, name string, fileType uint8) []byte {
	buf := make([]byte, 30+len(name))
	binary.LittleEndian.PutUint64(buf[0:8], childInode)
	buf[8] = BTRFS_INODE_ITEM_KEY
	binary.LittleEndian.PutUint64(buf[9:17], 0)
	binary.LittleEndian.PutUint64(buf[17:25], 0)
	binary.LittleEndian.PutUint16(buf[25:27], 0)
	binary.LittleEndian.PutUint16(buf[27:29], uint16(len(name)))
	buf[29] = fileType
	copy(buf[30:], name)
	return buf
}

func makeExtentDataInline(fileOffset uint64, data []byte) []byte {
	buf := make([]byte, 21+len(data))
	binary.LittleEndian.PutUint64(buf[0:8], 0)
	binary.LittleEndian.PutUint64(buf[8:16], uint64(len(data)))
	buf[16] = 0
	buf[17] = 0
	binary.LittleEndian.PutUint16(buf[18:20], 0)
	buf[20] = BTRFS_FILE_EXTENT_INLINE
	copy(buf[21:], data)
	_ = fileOffset
	return buf
}

func (b *FileSystem) createFile(path string, mode uint32, fileType uint8) error {
	dir, name, err := splitPath(path)
	if err != nil {
		return err
	}

	dirInode, err := b.resolvePath(dir)
	if err != nil {
		return err
	}

	maxInode := uint64(0)
	b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.typ == BTRFS_INODE_ITEM_KEY && item.key.objectid > maxInode {
			maxInode = item.key.objectid
		}
		_ = data
		return nil
	})
	newInode := maxInode + 1

	if err := b.insertInodeItem(newInode, mode); err != nil {
		return err
	}

	if _, err := b.lookupDirEntry(dirInode, name); err == nil {
		return fmt.Errorf("btrfs: entry already exists: %s", name)
	}

	nextOff := b.nextDirIndex(dirInode)
	dirData := makeDirItem(newInode, name, fileType)
	if err := b.insertIntoLeaf(b.fsRoot, key{
		objectid: dirInode,
		typ:      BTRFS_DIR_INDEX_KEY,
		offset:   nextOff,
	}, dirData); err != nil {
		return err
	}

	now := uint64(time.Now().Unix())
	_ = now
	b.pathCache.Delete(dir)
	return nil
}

func (b *FileSystem) insertInodeItem(inodeNum uint64, mode uint32) error {
	now := uint64(time.Now().Unix())
	data := make([]byte, 160)
	binary.LittleEndian.PutUint64(data[0:8], b.sb.Generation)
	binary.LittleEndian.PutUint64(data[8:16], b.sb.Generation)
	binary.LittleEndian.PutUint64(data[24:32], 0)
	binary.LittleEndian.PutUint32(data[40:44], 1)
	binary.LittleEndian.PutUint32(data[44:48], 0)
	binary.LittleEndian.PutUint32(data[48:52], 0)
	binary.LittleEndian.PutUint32(data[52:56], mode)
	binary.LittleEndian.PutUint32(data[56:60], 0)
	binary.LittleEndian.PutUint64(data[60:68], 0)
	binary.LittleEndian.PutUint64(data[96:104], now)
	binary.LittleEndian.PutUint32(data[104:108], 0)
	binary.LittleEndian.PutUint64(data[108:116], now)
	binary.LittleEndian.PutUint32(data[116:120], 0)
	binary.LittleEndian.PutUint64(data[120:128], now)
	binary.LittleEndian.PutUint32(data[128:132], 0)
	binary.LittleEndian.PutUint64(data[132:140], now)
	binary.LittleEndian.PutUint32(data[140:144], 0)
	return b.insertIntoLeaf(b.fsRoot, key{
		objectid: inodeNum,
		typ:      BTRFS_INODE_ITEM_KEY,
		offset:   0,
	}, data)
}

func (b *FileSystem) nextDirIndex(dirInode uint64) uint64 {
	maxIdx := uint64(0)
	b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.objectid == dirInode && item.key.typ == BTRFS_DIR_INDEX_KEY {
			if item.key.offset > maxIdx {
				maxIdx = item.key.offset
			}
		}
		_ = data
		return nil
	})
	return maxIdx + 1
}

func (b *FileSystem) updateInodeTime(inodeNum uint64, now uint64) {
	var data []byte
	b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == inodeNum && item.key.typ == BTRFS_INODE_ITEM_KEY {
			data = make([]byte, len(d))
			copy(data, d)
			return fmt.Errorf("stop")
		}
		return nil
	})
	if data == nil {
		return
	}
	binary.LittleEndian.PutUint64(data[120:128], now)
	binary.LittleEndian.PutUint64(data[108:116], now)
	b.insertIntoLeaf(b.fsRoot, key{
		objectid: inodeNum,
		typ:      BTRFS_INODE_ITEM_KEY,
		offset:   0,
	}, data)
}

func splitPath(path string) (parent, name string, err error) {
	if path == "" || path == "/" {
		return "", "", fmt.Errorf("cannot split root path")
	}
	idx := len(path) - 1
	for idx >= 0 && path[idx] == '/' {
		idx--
	}
	if idx < 0 {
		return "", "", fmt.Errorf("invalid path")
	}
	trimmed := path[:idx+1]
	idx = len(trimmed) - 1
	for idx >= 0 && trimmed[idx] != '/' {
		idx--
	}
	if idx <= 0 {
		parent = "/"
	} else {
		parent = trimmed[:idx]
	}
	name = trimmed[idx+1:]
	if name == "" {
		return "", "", fmt.Errorf("empty leaf name")
	}
	return parent, name, nil
}

func calcCrc32c(data []byte) uint32 {
	var crc uint32 = 0xFFFFFFFF
	for _, b := range data {
		crc = crc32cTable[byte(crc)^b] ^ (crc >> 8)
	}
	return crc ^ 0xFFFFFFFF
}

var crc32cTable [256]uint32

func init() {
	for i := 0; i < 256; i++ {
		crc := uint32(i)
		for j := 0; j < 8; j++ {
			if crc&1 != 0 {
				crc = 0x82F63B78 ^ (crc >> 1)
			} else {
				crc >>= 1
			}
		}
		crc32cTable[i] = crc
	}
}

func (b *FileSystem) deleteFromLeaf(logicalAddr uint64, k key) error {
	leafLogical, err := b.resolveLeaf(logicalAddr, k)
	if err != nil {
		return err
	}
	physAddr, err := b.tc.resolve(leafLogical)
	if err != nil {
		return err
	}
	bs := int(b.sb.NodeSize)
	buf := make([]byte, bs)
	if _, err := b.dev.ReadAt(buf, int64(physAddr)); err != nil {
		return fmt.Errorf("btrfs: read leaf: %w", err)
	}

	nritems := int(binary.LittleEndian.Uint32(buf[96:100]))

	type itemRec struct {
		key    key
		offset uint32
		size   uint32
		data   []byte
	}
	items := make([]itemRec, nritems)
	deleteIdx := -1
	for i := 0; i < nritems; i++ {
		off := nodeHeaderSize + i*25
		if off+25 > bs {
			items = items[:i]
			nritems = i
			break
		}
		items[i] = itemRec{
			key: key{
				objectid: binary.LittleEndian.Uint64(buf[off:]),
				typ:      buf[off+8],
				offset:   binary.LittleEndian.Uint64(buf[off+9:]),
			},
			offset: binary.LittleEndian.Uint32(buf[off+17:]),
			size:   binary.LittleEndian.Uint32(buf[off+21:]),
		}
		if items[i].key == k {
			deleteIdx = i
		}
		d := make([]byte, items[i].size)
		di := nodeHeaderSize + int(items[i].offset)
		if di < 0 || di+int(items[i].size) > bs {
			return fmt.Errorf("btrfs: item %d data out of bounds", i)
		}
		copy(d, buf[di:di+int(items[i].size)])
		items[i].data = d
	}

	if deleteIdx == -1 {
		return fmt.Errorf("btrfs: key not found for deletion: %+v", k)
	}

	// Rebuild items slice without the deleted item
	all := make([]itemRec, 0, nritems-1)
	for i := 0; i < nritems; i++ {
		if i == deleteIdx {
			continue
		}
		all = append(all, items[i])
	}

	// Build new leaf
	dst := make([]byte, bs)
	copy(dst[:nodeHeaderSize], buf[:nodeHeaderSize])

	// Write data area: fill from the end going backward
	dataPos := bs
	for i := len(all) - 1; i >= 0; i-- {
		sz := len(all[i].data)
		dataPos -= sz
		copy(dst[dataPos:dataPos+sz], all[i].data)
	}

	// Write item records
	curDataOff := dataPos
	for i, it := range all {
		off := nodeHeaderSize + i*25
		encodeKeyInto(dst[off:off+17], it.key)
		relOff := curDataOff - nodeHeaderSize
		binary.LittleEndian.PutUint32(dst[off+17:off+21], uint32(relOff))
		binary.LittleEndian.PutUint32(dst[off+21:off+25], uint32(len(it.data)))
		curDataOff += len(it.data)
	}

	binary.LittleEndian.PutUint32(dst[96:100], uint32(len(all)))
	binary.LittleEndian.PutUint64(dst[80:88], b.sb.Generation)
	binary.LittleEndian.PutUint64(dst[48:56], uint64(leafLogical))

	csum := calcCrc32c(dst[32:])
	binary.LittleEndian.PutUint32(dst[0:4], csum)

	if _, err := b.dev.WriteAt(dst, int64(physAddr)); err != nil {
		return fmt.Errorf("btrfs: write leaf: %w", err)
	}
	return nil
}

func (b *FileSystem) updateInLeaf(logicalAddr uint64, k key, itemData []byte) error {
	leafLogical, err := b.resolveLeaf(logicalAddr, k)
	if err != nil {
		return err
	}
	physAddr, err := b.tc.resolve(leafLogical)
	if err != nil {
		return err
	}
	bs := int(b.sb.NodeSize)
	buf := make([]byte, bs)
	if _, err := b.dev.ReadAt(buf, int64(physAddr)); err != nil {
		return fmt.Errorf("btrfs: read leaf: %w", err)
	}

	nritems := int(binary.LittleEndian.Uint32(buf[96:100]))

	type itemRec struct {
		key    key
		offset uint32
		size   uint32
		data   []byte
	}
	items := make([]itemRec, nritems)
	updateIdx := -1
	for i := 0; i < nritems; i++ {
		off := nodeHeaderSize + i*25
		if off+25 > bs {
			items = items[:i]
			nritems = i
			break
		}
		items[i] = itemRec{
			key: key{
				objectid: binary.LittleEndian.Uint64(buf[off:]),
				typ:      buf[off+8],
				offset:   binary.LittleEndian.Uint64(buf[off+9:]),
			},
			offset: binary.LittleEndian.Uint32(buf[off+17:]),
			size:   binary.LittleEndian.Uint32(buf[off+21:]),
		}
		if items[i].key == k {
			updateIdx = i
		}
		d := make([]byte, items[i].size)
		di := nodeHeaderSize + int(items[i].offset)
		if di < 0 || di+int(items[i].size) > bs {
			return fmt.Errorf("btrfs: item %d data out of bounds", i)
		}
		copy(d, buf[di:di+int(items[i].size)])
		items[i].data = d
	}

	if updateIdx == -1 {
		return fmt.Errorf("btrfs: key not found for update: %+v", k)
	}

	// Update the item data
	items[updateIdx].data = itemData

	// Calculate total data size
	totalData := 0
	for _, it := range items {
		totalData += len(it.data)
	}

	// Check space
	needed := nodeHeaderSize + len(items)*25 + totalData
	if needed > bs {
		return fmt.Errorf("btrfs: leaf has no space for update (need %d, have %d)", needed, bs)
	}

	// Build new leaf
	dst := make([]byte, bs)
	copy(dst[:nodeHeaderSize], buf[:nodeHeaderSize])

	// Write data area: fill from the end going backward
	dataPos := bs
	for i := len(items) - 1; i >= 0; i-- {
		sz := len(items[i].data)
		dataPos -= sz
		copy(dst[dataPos:dataPos+sz], items[i].data)
	}

	// Write item records
	curDataOff := dataPos
	for i, it := range items {
		off := nodeHeaderSize + i*25
		encodeKeyInto(dst[off:off+17], it.key)
		relOff := curDataOff - nodeHeaderSize
		binary.LittleEndian.PutUint32(dst[off+17:off+21], uint32(relOff))
		binary.LittleEndian.PutUint32(dst[off+21:off+25], uint32(len(it.data)))
		curDataOff += len(it.data)
	}

	binary.LittleEndian.PutUint32(dst[96:100], uint32(len(items)))
	binary.LittleEndian.PutUint64(dst[80:88], b.sb.Generation)
	binary.LittleEndian.PutUint64(dst[48:56], uint64(leafLogical))

	csum := calcCrc32c(dst[32:])
	binary.LittleEndian.PutUint32(dst[0:4], csum)

	if _, err := b.dev.WriteAt(dst, int64(physAddr)); err != nil {
		return fmt.Errorf("btrfs: write leaf: %w", err)
	}
	return nil
}

func (b *FileSystem) findLeaf(root uint64, lvl uint8, k key) (uint64, error) {
	if lvl == 0 {
		return root, nil
	}
	phys, err := b.tc.resolve(root)
	if err != nil {
		return 0, err
	}
	buf, err := readNode(b.dev, phys, b.sb.NodeSize)
	if err != nil {
		return 0, err
	}
	r := &byteReader{buf: buf}
	h := decodeNodeHeader(r)
	if h.level == 0 {
		return root, nil
	}

	var bestPtr uint64 = 0
	for i := uint32(0); i < h.nritems; i++ {
		p := decodeInternalPtr(r)
		if i == 0 || !k.less(p.key) {
			bestPtr = p.blockptr
		}
	}
	if bestPtr == 0 {
		return root, nil
	}
	return b.findLeaf(bestPtr, h.level-1, k)
}

func (b *FileSystem) resolveLeaf(logicalAddr uint64, k key) (uint64, error) {
	var lvl uint8
	if logicalAddr == b.fsRoot {
		lvl = b.fsRootLvl
	} else if logicalAddr == b.extentRoot {
		lvl = b.extentRootLvl
	} else if logicalAddr == b.sb.ChunkRoot {
		lvl = b.sb.ChunkRootLevel
	} else {
		return logicalAddr, nil
	}
	return b.findLeaf(logicalAddr, lvl, k)
}

type extentRange struct {
	logical uint64
	length  uint64
}

func (b *FileSystem) allocateSpace(size uint64, bgType uint64) (logical uint64, physical uint64, err error) {
	var targetChunk *chunkMapping
	for i := range b.tc.chunks {
		c := &b.tc.chunks[i]
		if c.flags&bgType != 0 {
			targetChunk = c
			break
		}
	}
	if targetChunk == nil {
		if len(b.tc.chunks) > 0 {
			targetChunk = &b.tc.chunks[0]
		} else {
			return 0, 0, fmt.Errorf("btrfs: no chunk mappings found for allocation")
		}
	}

	var occupied []extentRange

	_ = b.walkExtentTree(func(item leafItem, data []byte) error {
		if item.key.typ == BTRFS_EXTENT_ITEM_KEY {
			occupied = append(occupied, extentRange{
				logical: item.key.objectid,
				length:  item.key.offset,
			})
		} else if item.key.typ == 169 {
			occupied = append(occupied, extentRange{
				logical: item.key.objectid,
				length:  uint64(b.sb.NodeSize),
			})
		}
		return nil
	})

	_ = b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.typ == BTRFS_EXTENT_DATA_KEY {
			if len(data) >= 45 && data[20] != BTRFS_FILE_EXTENT_INLINE {
				diskBytenr := binary.LittleEndian.Uint64(data[21:29])
				diskNumBytes := binary.LittleEndian.Uint64(data[29:37])
				if diskBytenr > 0 && diskNumBytes > 0 {
					occupied = append(occupied, extentRange{
						logical: diskBytenr,
						length:  diskNumBytes,
					})
				}
			}
		}
		return nil
	})

	sort.Slice(occupied, func(i, j int) bool {
		return occupied[i].logical < occupied[j].logical
	})

	sectorSize := uint64(b.sb.SectorSize)
	if sectorSize == 0 {
		sectorSize = 4096
	}
	alignedSize := ((size + sectorSize - 1) / sectorSize) * sectorSize

	candidate := targetChunk.logical
	for _, occ := range occupied {
		if occ.logical+occ.length <= candidate {
			continue
		}
		if occ.logical >= candidate+alignedSize {
			break
		}
		candidate = occ.logical + occ.length
		candidate = ((candidate + sectorSize - 1) / sectorSize) * sectorSize
	}

	if candidate+alignedSize <= targetChunk.logical+targetChunk.length {
		logical = candidate
		physical = targetChunk.physical + (logical - targetChunk.logical)
		return logical, physical, nil
	}

	return 0, 0, fmt.Errorf("btrfs: out of disk space in block group %d", bgType)
}

func (b *FileSystem) registerExtent(logicalAddr uint64, length uint64) error {
	if b.extentRoot == 0 {
		return nil
	}
	payload := make([]byte, 24)
	binary.LittleEndian.PutUint64(payload[0:8], 1)
	binary.LittleEndian.PutUint64(payload[8:16], b.sb.Generation)
	binary.LittleEndian.PutUint64(payload[16:24], 1)

	return b.insertIntoLeaf(b.extentRoot, key{
		objectid: logicalAddr,
		typ:      BTRFS_EXTENT_ITEM_KEY,
		offset:   length,
	}, payload)
}

func (b *FileSystem) writeItemsToLeaf(leafLogical uint64, items []itemEntry, bs int) error {
	physAddr, err := b.tc.resolve(leafLogical)
	if err != nil {
		return err
	}
	buf := make([]byte, bs)
	if _, err := b.dev.ReadAt(buf, int64(physAddr)); err != nil {
		buf = make([]byte, bs)
	}

	dst := make([]byte, bs)
	copy(dst[:nodeHeaderSize], buf[:nodeHeaderSize])

	dataPos := bs
	for i := len(items) - 1; i >= 0; i-- {
		sz := len(items[i].data)
		dataPos -= sz
		copy(dst[dataPos:dataPos+sz], items[i].data)
	}

	curDataOff := dataPos
	for i, it := range items {
		off := nodeHeaderSize + i*25
		encodeKeyInto(dst[off:off+17], it.k)
		relOff := curDataOff - nodeHeaderSize
		binary.LittleEndian.PutUint32(dst[off+17:off+21], uint32(relOff))
		binary.LittleEndian.PutUint32(dst[off+21:off+25], uint32(len(it.data)))
		curDataOff += len(it.data)
	}

	binary.LittleEndian.PutUint32(dst[96:100], uint32(len(items)))
	binary.LittleEndian.PutUint64(dst[80:88], b.sb.Generation)
	binary.LittleEndian.PutUint64(dst[48:56], uint64(leafLogical))

	csum := calcCrc32c(dst[32:])
	binary.LittleEndian.PutUint32(dst[0:4], csum)

	_, err = b.dev.WriteAt(dst, int64(physAddr))
	return err
}

func (b *FileSystem) splitLeaf(logicalLeaf uint64, k key, all []itemEntry, bs int, ins int) error {
	newLeafLogical, _, err := b.allocateSpace(uint64(bs), BTRFS_BLOCK_GROUP_METADATA)
	if err != nil {
		return fmt.Errorf("btrfs: split leaf: fail to allocate node: %w", err)
	}

	mid := len(all) / 2
	leftItems := all[:mid]
	rightItems := all[mid:]

	err = b.writeItemsToLeaf(logicalLeaf, leftItems, bs)
	if err != nil {
		return err
	}

	err = b.writeItemsToLeaf(newLeafLogical, rightItems, bs)
	if err != nil {
		return err
	}

	_ = b.registerExtent(newLeafLogical, uint64(bs))

	if b.fsRoot == logicalLeaf && b.fsRootLvl == 0 {
		newRootLogical, newRootPhys, err := b.allocateSpace(uint64(bs), BTRFS_BLOCK_GROUP_METADATA)
		if err != nil {
			return err
		}

		dst := make([]byte, bs)
		binary.LittleEndian.PutUint32(dst[96:100], 2)
		dst[100] = 1
		binary.LittleEndian.PutUint64(dst[80:88], b.sb.Generation)
		binary.LittleEndian.PutUint64(dst[48:56], newRootLogical)
		binary.LittleEndian.PutUint64(dst[112:120], 5)

		off1 := nodeHeaderSize
		encodeKeyInto(dst[off1:off1+17], leftItems[0].k)
		binary.LittleEndian.PutUint64(dst[off1+17:off1+25], logicalLeaf)
		binary.LittleEndian.PutUint64(dst[off1+25:off1+33], b.sb.Generation)

		off2 := nodeHeaderSize + 33
		encodeKeyInto(dst[off2:off2+17], rightItems[0].k)
		binary.LittleEndian.PutUint64(dst[off2+17:off2+25], newLeafLogical)
		binary.LittleEndian.PutUint64(dst[off2+25:off2+33], b.sb.Generation)

		csum := calcCrc32c(dst[32:])
		binary.LittleEndian.PutUint32(dst[0:4], csum)

		if _, err := b.dev.WriteAt(dst, int64(newRootPhys)); err != nil {
			return err
		}

		_ = b.registerExtent(newRootLogical, uint64(bs))

		b.fsRoot = newRootLogical
		b.fsRootLvl = 1

		return nil
	}

	return fmt.Errorf("btrfs: leaf has no space (need %d, have %d) and split not supported above level 0", nodeHeaderSize+len(all)*25, bs)
}

