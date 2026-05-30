package btrfs

import (
	"fmt"
)

type chunkMapping struct {
	logical  uint64
	length   uint64
	physical uint64
}

type treeContext struct {
	dev      readerAt
	nodeSize uint32
	chunks   []chunkMapping
}

func (tc *treeContext) readNodeAt(logical uint64) ([]byte, error) {
	phys, err := tc.resolve(logical)
	if err != nil {
		return nil, err
	}
	buf, err := readNode(tc.dev, phys, tc.nodeSize)
	if err != nil {
		return nil, fmt.Errorf("read node at logical 0x%x -> phys 0x%x: %w", logical, phys, err)
	}
	return buf, nil
}

func (tc *treeContext) resolve(logical uint64) (uint64, error) {
	for _, c := range tc.chunks {
		if logical >= c.logical && logical < c.logical+c.length {
			return c.physical + (logical - c.logical), nil
		}
	}
	return 0, fmt.Errorf("btrfs: no chunk mapping for logical 0x%x", logical)
}

func (tc *treeContext) addChunk(logical, length, physical uint64) {
	tc.chunks = append(tc.chunks, chunkMapping{logical: logical, length: length, physical: physical})
}

func (tc *treeContext) resolveChunkTree(rootAddr uint64, rootLevel uint8) error {
	return tc.walkTree(rootAddr, rootLevel, func(item leafItem, data []byte) error {
		if item.key.typ == BTRFS_CHUNK_ITEM_KEY {
			return tc.parseChunkItem(item, data)
		}
		return nil
	})
}

func (tc *treeContext) parseChunkItem(item leafItem, data []byte) error {
	if len(data) < 48 {
		return nil
	}
	r := &byteReader{buf: data}
	length := r.u64()
	_ = r.u64() // owner
	_ = r.u64() // stripe_len
	_ = r.u64() // type (flags)
	_ = r.u32() // io_align
	_ = r.u32() // io_width
	_ = r.u32() // sector_size
	numStripes := r.u16()
	_ = r.u16() // sub_stripes

	logical := item.key.offset

	if numStripes == 0 || len(data) < 48+int(numStripes)*32 {
		return nil
	}

	r.skip(int(numStripes-1) * 32)
	_ = r.u64() // device_id of last stripe
	physOffset := r.u64()

	tc.addChunk(logical, length, physOffset)
	return nil
}

func (tc *treeContext) walkTree(addr uint64, level uint8, cb func(leafItem, []byte) error) error {
	return tc.walkTreeWithNodeCB(addr, level, cb, nil)
}

func (tc *treeContext) walkTreeWithNodeCB(addr uint64, level uint8, cb func(leafItem, []byte) error, nodeCB func(uint64)) error {
	phys, err := tc.resolve(addr)
	if err != nil {
		return err
	}
	if nodeCB != nil {
		nodeCB(phys)
	}

	buf, err := readNode(tc.dev, phys, tc.nodeSize)
	if err != nil {
		return fmt.Errorf("btrfs: read node at 0x%x: %w", addr, err)
	}

	r := &byteReader{buf: buf}
	h := decodeNodeHeader(r)

	if h.level == 0 {
		for i := uint32(0); i < h.nritems; i++ {
			item := decodeLeafItem(r)
			dataStart := nodeHeaderSize + item.offset
			dataEnd := dataStart + item.size
			if dataEnd > uint32(len(buf)) {
				return fmt.Errorf("btrfs: item %d data out of bounds: off=%d size=%d bufLen=%d", i, dataStart, item.size, len(buf))
			}
			data := buf[dataStart:dataEnd]
			if err := cb(item, data); err != nil {
				return err
			}
		}
	} else {
		for i := uint32(0); i < h.nritems; i++ {
			ptr := decodeInternalPtr(r)
			if err := tc.walkTreeWithNodeCB(ptr.blockptr, h.level-1, cb, nodeCB); err != nil {
				return err
			}
		}
	}
	return nil
}
