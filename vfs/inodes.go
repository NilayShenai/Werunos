package vfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
)

type Inode struct {

	I_mode uint16

	I_uid uint16

	I_size_lo uint32

	I_atime uint32

	I_ctime uint32

	I_mtime uint32

	I_dtime uint32

	I_gid uint16

	I_links_count uint16

	I_blocks_lo uint32

	I_flags uint32

	L_i_version uint32

	I_block [60]byte

	I_generation uint32

	I_file_acl_lo uint32

	I_size_high uint32

	I_obso_faddr uint32

	L_i_blocks_high uint16

	L_i_file_acl_high uint16

	L_i_uid_high uint16

	L_i_gid_high uint16

	L_i_checksum_lo uint16

	L_i_reserved uint16

	I_extra_isize uint16

	I_checksum_hi uint16

	I_ctime_extra uint32

	I_mtime_extra uint32

	I_atime_extra uint32

	I_crtime uint32

	I_crtime_extra uint32

	I_version_hi uint32

	I_projid uint32
}

const (
	RootInodeNum = 2

	S_IXOTH  = 0x1
	S_IWOTH  = 0x2
	S_IROTH  = 0x4
	S_IXGRP  = 0x8
	S_IWGRP  = 0x10
	S_IRGRP  = 0x20
	S_IXUSR  = 0x40
	S_IWUSR  = 0x80
	S_IRUSR  = 0x100
	S_ISVTX  = 0x200
	S_ISGID  = 0x400
	S_ISUID  = 0x800
	S_IFIFO  = 0x1000
	S_IFCHR  = 0x2000
	S_IFDIR  = 0x4000
	S_IFBLK  = 0x6000
	S_IFREG  = 0x8000
	S_IFLNK  = 0xA000
	S_IFSOCK = 0xC000

	EXT4_SECRM_FL            = 0x1
	EXT4_UNRM_FL             = 0x2
	EXT4_COMPR_FL            = 0x4
	EXT4_SYNC_FL             = 0x8
	EXT4_IMMUTABLE_FL        = 0x10
	EXT4_APPEND_FL           = 0x20
	EXT4_NODUMP_FL           = 0x40
	EXT4_NOATIME_FL          = 0x80
	EXT4_DIRTY_FL            = 0x100
	EXT4_COMPRBLK_FL         = 0x200
	EXT4_NOCOMPR_FL          = 0x400
	EXT4_ENCRYPT_FL          = 0x800
	EXT4_INDEX_FL            = 0x1000
	EXT4_IMAGIC_FL           = 0x2000
	EXT4_JOURNAL_DATA_FL     = 0x4000
	EXT4_NOTAIL_FL           = 0x8000
	EXT4_DIRSYNC_FL          = 0x10000
	EXT4_TOPDIR_FL           = 0x20000
	EXT4_HUGE_FILE_FL        = 0x40000
	EXT4_EXTENTS_FL          = 0x80000
	EXT4_VERITY_FL           = 0x100000
	EXT4_EA_INODE_FL         = 0x200000
	EXT4_EOFBLOCKS_FL        = 0x400000
	EXT4_SNAPFILE_FL         = 0x01000000
	EXT4_SNAPFILE_DELETED_FL = 0x04000000
	EXT4_SNAPFILE_SHRUNK_FL  = 0x08000000
	EXT4_INLINE_DATA_FL      = 0x10000000
	EXT4_PROJINHERIT_FL      = 0x20000000
	EXT4_CASEFOLD_FL         = 0x40000000
	EXT4_RESERVED_FL         = 0x80000000
)

func (fs *FileSystem) ReadInode(inodeNum uint32) (*Inode, error) {
	if inodeNum < 1 {
		return nil, fmt.Errorf("invalid inode number: %d (inodes start at 1)", inodeNum)
	}

	groupIndex := (inodeNum - 1) / fs.sb.S_inodes_per_group

	localInodeIndex := (inodeNum - 1) % fs.sb.S_inodes_per_group

	if groupIndex >= fs.GroupCount {
		return nil, fmt.Errorf(
			"inode %d belongs to group %d, but we only have %d groups",
			inodeNum, groupIndex, fs.GroupCount,
		)
	}

	bgd := fs.Bgds[groupIndex]
	if bgd.BG_inode_table_lo == 0 {
		return nil, fmt.Errorf("inode table not found in group %d", groupIndex)
	}

	tableBlock := uint64(bgd.BG_inode_table_lo)
	if fs.DescSize > 32 {
		tableBlock |= uint64(bgd.BG_inode_table_hi) << 32
	}

	offset := (tableBlock * fs.BlockSize) + (uint64(localInodeIndex) * uint64(fs.InodeSize))

	if offset > math.MaxInt64 {
		return nil, fmt.Errorf("inode offset %d overflows int64", offset)
	}

	var inode Inode
	buf := make([]byte, fs.InodeSize)
	_, err := fs.dev.ReadAt(buf, int64(offset))
	if err != nil {
		return nil, fmt.Errorf("failed to read inode %d at offset %d: %w", inodeNum, offset, err)
	}

	err = binary.Read(bytes.NewReader(buf), binary.LittleEndian, &inode)
	if err != nil {
		return nil, fmt.Errorf("failed to decode inode %d: %w", inodeNum, err)
	}

	return &inode, nil
}

func (fs *FileSystem) ReadRootInode() (*Inode, error) {

	return fs.ReadInode(RootInodeNum)
}

const ifmt = 0xF000

func (i *Inode) IsDir() bool {
	return i.I_mode&ifmt == S_IFDIR
}

func (i *Inode) IsRegular() bool {
	return i.I_mode&ifmt == S_IFREG
}

func (i *Inode) IsSymlink() bool {
	return i.I_mode&ifmt == S_IFLNK
}

func (i *Inode) Size() uint64 {
	if i.IsDir() {

		return uint64(i.I_size_lo)
	}
	return (uint64(i.I_size_high) << 32) | uint64(i.I_size_lo)
}

func (i *Inode) UsesExtents() bool {
	return i.I_flags&EXT4_EXTENTS_FL != 0
}
