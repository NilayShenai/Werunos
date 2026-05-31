package vfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"
)

var ErrAttrNotExist = fmt.Errorf("attribute does not exist")
var ErrNoSpace = fmt.Errorf("no space available")

var xattrPrefixes = []struct {
	Index  uint8
	Prefix string
}{
	{2, "system.posix_acl_access"},
	{3, "system.posix_acl_default"},
	{1, "user."},
	{4, "trusted."},
	{6, "security."},
	{7, "system."},
	{8, "system.richacl"},
}

func splitXAttrName(fullName string) (uint8, string) {
	for _, xp := range xattrPrefixes {
		if len(fullName) >= len(xp.Prefix) && fullName[:len(xp.Prefix)] == xp.Prefix {
			return xp.Index, fullName[len(xp.Prefix):]
		}
	}
	return 0, fullName
}

func joinXAttrName(index uint8, suffix string) string {
	for _, xp := range xattrPrefixes {
		if xp.Index == index {
			return xp.Prefix + suffix
		}
	}
	return suffix
}

type serializedEntry struct {
	nameLen   uint8
	nameIndex uint8
	name      string
	value     []byte
}

func (fs *FileSystem) ReadRawInode(inodeNum uint32) ([]byte, error) {
	if inodeNum < 1 {
		return nil, fmt.Errorf("invalid inode number: %d", inodeNum)
	}
	groupIndex := (inodeNum - 1) / fs.sb.S_inodes_per_group
	localInodeIndex := (inodeNum - 1) % fs.sb.S_inodes_per_group
	if groupIndex >= fs.GroupCount {
		return nil, fmt.Errorf("inode %d group out of range", inodeNum)
	}
	bgd := fs.Bgds[groupIndex]
	tableBlock := uint64(bgd.BG_inode_table_lo)
	if fs.DescSize > 32 {
		tableBlock |= uint64(bgd.BG_inode_table_hi) << 32
	}
	offset := (tableBlock * fs.BlockSize) + (uint64(localInodeIndex) * uint64(fs.InodeSize))
	buf := make([]byte, fs.InodeSize)
	if _, err := fs.dev.ReadAt(buf, int64(offset)); err != nil {
		return nil, err
	}
	return buf, nil
}

func (fs *FileSystem) WriteRawInode(inodeNum uint32, buf []byte) error {
	if inodeNum < 1 {
		return fmt.Errorf("invalid inode number: %d", inodeNum)
	}
	groupIndex := (inodeNum - 1) / fs.sb.S_inodes_per_group
	localInodeIndex := (inodeNum - 1) % fs.sb.S_inodes_per_group
	if groupIndex >= fs.GroupCount {
		return fmt.Errorf("inode %d group out of range", inodeNum)
	}
	bgd := fs.Bgds[groupIndex]
	tableBlock := uint64(bgd.BG_inode_table_lo)
	if fs.DescSize > 32 {
		tableBlock |= uint64(bgd.BG_inode_table_hi) << 32
	}
	offset := (tableBlock * fs.BlockSize) + (uint64(localInodeIndex) * uint64(fs.InodeSize))
	_, err := fs.dev.WriteAt(buf, int64(offset))
	return err
}

func (fs *FileSystem) ListXAttrs(inodeNum uint32) (map[string][]byte, error) {
	xattrs := make(map[string][]byte)

	rawInode, err := fs.ReadRawInode(inodeNum)
	if err != nil {
		return nil, err
	}

	var inode Inode
	if err := binary.Read(bytes.NewReader(rawInode), binary.LittleEndian, &inode); err != nil {
		return nil, err
	}

	if inode.I_extra_isize > 0 && int(128+inode.I_extra_isize+4) <= len(rawInode) {
		ibodyOffset := 128 + int(inode.I_extra_isize)
		magic := binary.LittleEndian.Uint32(rawInode[ibodyOffset : ibodyOffset+4])
		if magic == 0xEA020000 {
			entryStart := ibodyOffset + 4
			curr := entryStart
			for {
				if curr+4 > len(rawInode) {
					break
				}
				if binary.LittleEndian.Uint32(rawInode[curr:curr+4]) == 0 {
					break
				}
				if curr+16 > len(rawInode) {
					break
				}

				e_name_len := rawInode[curr]
				e_name_index := rawInode[curr+1]
				e_value_offs := binary.LittleEndian.Uint16(rawInode[curr+2 : curr+4])
				e_value_inum := binary.LittleEndian.Uint32(rawInode[curr+4 : curr+8])
				e_value_size := binary.LittleEndian.Uint32(rawInode[curr+8 : curr+12])

				entryLen := (16 + int(e_name_len) + 3) &^ 3
				if curr+entryLen > len(rawInode) {
					break
				}

				name := string(rawInode[curr+16 : curr+16+int(e_name_len)])
				fullName := joinXAttrName(e_name_index, name)

				valOff := entryStart + int(e_value_offs)
				if valOff+int(e_value_size) <= len(rawInode) {
					val := make([]byte, e_value_size)
					copy(val, rawInode[valOff:valOff+int(e_value_size)])
					xattrs[fullName] = val
				}
				_ = e_value_inum

				curr += entryLen
			}
		}
	}

	fileAclBlock := (uint64(inode.L_i_file_acl_high) << 32) | uint64(inode.I_file_acl_lo)
	if fileAclBlock > 0 {
		blockBuf := make([]byte, fs.BlockSize)
		if _, err := fs.dev.ReadAt(blockBuf, int64(fileAclBlock*fs.BlockSize)); err == nil {
			magic := binary.LittleEndian.Uint32(blockBuf[0:4])
			if magic == 0xEA020000 {
				curr := 32
				for {
					if curr+4 > len(blockBuf) {
						break
					}
					if binary.LittleEndian.Uint32(blockBuf[curr:curr+4]) == 0 {
						break
					}
					if curr+16 > len(blockBuf) {
						break
					}

					e_name_len := blockBuf[curr]
					e_name_index := blockBuf[curr+1]
					e_value_offs := binary.LittleEndian.Uint16(blockBuf[curr+2 : curr+4])
					e_value_inum := binary.LittleEndian.Uint32(blockBuf[curr+4 : curr+8])
					e_value_size := binary.LittleEndian.Uint32(blockBuf[curr+8 : curr+12])

					entryLen := (16 + int(e_name_len) + 3) &^ 3
					if curr+entryLen > len(blockBuf) {
						break
					}

					name := string(blockBuf[curr+16 : curr+16+int(e_name_len)])
					fullName := joinXAttrName(e_name_index, name)

					valOff := int(e_value_offs)
					if valOff+int(e_value_size) <= len(blockBuf) {
						val := make([]byte, e_value_size)
						copy(val, blockBuf[valOff:valOff+int(e_value_size)])
						xattrs[fullName] = val
					}
					_ = e_value_inum

					curr += entryLen
				}
			}
		}
	}

	return xattrs, nil
}

func serializeXAttrs(xattrs map[string][]byte, availSpace int, isBlock bool) ([]byte, error) {
	if len(xattrs) == 0 {
		return nil, nil
	}

	var entries []serializedEntry
	for fullName, val := range xattrs {
		idx, suffix := splitXAttrName(fullName)
		entries = append(entries, serializedEntry{
			nameLen:   uint8(len(suffix)),
			nameIndex: idx,
			name:      suffix,
			value:     val,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].nameIndex != entries[j].nameIndex {
			return entries[i].nameIndex < entries[j].nameIndex
		}
		if entries[i].nameLen != entries[j].nameLen {
			return entries[i].nameLen < entries[j].nameLen
		}
		return entries[i].name < entries[j].name
	})

	entryTableLen := 0
	for _, e := range entries {
		entryTableLen += (16 + int(e.nameLen) + 3) &^ 3
	}
	entryTableLen += 4

	valueTableLen := 0
	for _, e := range entries {
		valueTableLen += (len(e.value) + 3) &^ 3
	}

	if entryTableLen+valueTableLen > availSpace {
		return nil, ErrNoSpace
	}

	buf := make([]byte, availSpace)
	currEntryOff := 0
	currValOff := availSpace

	for _, e := range entries {
		valSizeAligned := (len(e.value) + 3) &^ 3
		currValOff -= valSizeAligned
		valueOffset := uint16(currValOff)

		buf[currEntryOff] = e.nameLen
		buf[currEntryOff+1] = e.nameIndex
		binary.LittleEndian.PutUint16(buf[currEntryOff+2:currEntryOff+4], valueOffset)
		binary.LittleEndian.PutUint32(buf[currEntryOff+4:currEntryOff+8], 0)
		binary.LittleEndian.PutUint32(buf[currEntryOff+8:currEntryOff+12], uint32(len(e.value)))
		binary.LittleEndian.PutUint32(buf[currEntryOff+12:currEntryOff+16], 0)

		copy(buf[currEntryOff+16:currEntryOff+16+int(e.nameLen)], []byte(e.name))
		copy(buf[currValOff:currValOff+len(e.value)], e.value)

		entryLen := (16 + int(e.nameLen) + 3) &^ 3
		currEntryOff += entryLen
	}

	return buf, nil
}

func (fs *FileSystem) GetXAttr(inodeNum uint32, name string) ([]byte, error) {
	xattrs, err := fs.ListXAttrs(inodeNum)
	if err != nil {
		return nil, err
	}
	val, ok := xattrs[name]
	if !ok {
		return nil, ErrAttrNotExist
	}
	return val, nil
}

func (fs *FileSystem) SetXAttr(inodeNum uint32, name string, value []byte) error {
	xattrs, err := fs.ListXAttrs(inodeNum)
	if err != nil {
		return err
	}

	if value == nil {
		if _, ok := xattrs[name]; !ok {
			return ErrAttrNotExist
		}
		delete(xattrs, name)
	} else {
		xattrs[name] = value
	}

	rawInode, err := fs.ReadRawInode(inodeNum)
	if err != nil {
		return err
	}

	var inode Inode
	if err := binary.Read(bytes.NewReader(rawInode), binary.LittleEndian, &inode); err != nil {
		return err
	}

	oldFileAclBlock := (uint64(inode.L_i_file_acl_high) << 32) | uint64(inode.I_file_acl_lo)

	inInodeFits := false
	var inInodeBuf []byte
	if inode.I_extra_isize > 0 {
		availSpace := int(fs.InodeSize) - 128 - int(inode.I_extra_isize) - 4
		if availSpace > 0 {
			inInodeBuf, err = serializeXAttrs(xattrs, availSpace, false)
			if err == nil {
				inInodeFits = true
			}
		}
	}

	if inInodeFits {
		inode.I_file_acl_lo = 0
		inode.L_i_file_acl_high = 0

		var inodeBuf bytes.Buffer
		_ = binary.Write(&inodeBuf, binary.LittleEndian, &inode)
		copy(rawInode, inodeBuf.Bytes())

		ibodyOffset := 128 + int(inode.I_extra_isize)
		if len(xattrs) > 0 {
			binary.LittleEndian.PutUint32(rawInode[ibodyOffset:ibodyOffset+4], 0xEA020000)
			copy(rawInode[ibodyOffset+4:], inInodeBuf)
		} else {
			for i := ibodyOffset; i < int(fs.InodeSize); i++ {
				rawInode[i] = 0
			}
		}

		if err := fs.WriteRawInode(inodeNum, rawInode); err != nil {
			return err
		}

		if oldFileAclBlock > 0 {
			if err := fs.FreeBlock(oldFileAclBlock); err != nil {
				return err
			}
		}

		return nil
	}

	if inode.I_extra_isize > 0 {
		ibodyOffset := 128 + int(inode.I_extra_isize)
		for i := ibodyOffset; i < int(fs.InodeSize); i++ {
			rawInode[i] = 0
		}
	}

	if len(xattrs) == 0 {
		inode.I_file_acl_lo = 0
		inode.L_i_file_acl_high = 0

		var inodeBuf bytes.Buffer
		_ = binary.Write(&inodeBuf, binary.LittleEndian, &inode)
		copy(rawInode, inodeBuf.Bytes())

		if err := fs.WriteRawInode(inodeNum, rawInode); err != nil {
			return err
		}

		if oldFileAclBlock > 0 {
			if err := fs.FreeBlock(oldFileAclBlock); err != nil {
				return err
			}
		}
		return nil
	}

	var blockNum uint64
	if oldFileAclBlock > 0 {
		blockNum = oldFileAclBlock
	} else {
		blockNum, err = fs.AllocateBlock()
		if err != nil {
			return err
		}
	}

	availSpace := int(fs.BlockSize) - 32
	blockBuf, err := serializeXAttrs(xattrs, availSpace, true)
	if err != nil {
		if oldFileAclBlock == 0 {
			_ = fs.FreeBlock(blockNum)
		}
		return err
	}

	fullBlockBuf := make([]byte, fs.BlockSize)
	binary.LittleEndian.PutUint32(fullBlockBuf[0:4], 0xEA020000)
	binary.LittleEndian.PutUint32(fullBlockBuf[4:8], 1)
	binary.LittleEndian.PutUint32(fullBlockBuf[8:12], 1)
	binary.LittleEndian.PutUint32(fullBlockBuf[12:16], 0)
	binary.LittleEndian.PutUint32(fullBlockBuf[16:20], 0)

	copy(fullBlockBuf[32:], blockBuf)

	if _, err := fs.dev.WriteAt(fullBlockBuf, int64(blockNum*fs.BlockSize)); err != nil {
		if oldFileAclBlock == 0 {
			_ = fs.FreeBlock(blockNum)
		}
		return err
	}

	inode.I_file_acl_lo = uint32(blockNum & 0xFFFFFFFF)
	inode.L_i_file_acl_high = uint16(blockNum >> 32)

	if oldFileAclBlock == 0 {
		blockSize := fs.BlockSize
		sectorCount := (uint64(inode.I_blocks_lo) | (uint64(inode.L_i_blocks_high) << 32)) + (blockSize / 512)
		inode.I_blocks_lo = uint32(sectorCount & 0xFFFFFFFF)
		inode.L_i_blocks_high = uint16(sectorCount >> 32)
	}

	var inodeBuf bytes.Buffer
	_ = binary.Write(&inodeBuf, binary.LittleEndian, &inode)
	copy(rawInode, inodeBuf.Bytes())

	return fs.WriteRawInode(inodeNum, rawInode)
}

func (fs *FileSystem) RemoveXAttr(inodeNum uint32, name string) error {
	return fs.SetXAttr(inodeNum, name, nil)
}
