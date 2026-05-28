package vfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

type SuperBlock struct {

	S_inodes_count uint32

	S_blocks_count_lo uint32

	S_r_blocks_count_lo uint32

	S_free_blocks_count_lo uint32

	S_free_inodes_count uint32

	S_first_data_block uint32

	S_log_block_size uint32

	S_log_cluster_size uint32

	S_blocks_per_group uint32

	S_clusters_per_group uint32

	S_inodes_per_group uint32

	S_mtime uint32

	S_wtime uint32

	S_mnt_count uint16

	S_max_mnt_count uint16

	S_magic uint16

	S_state uint16

	S_errors uint16

	S_minor_rev_level uint16

	S_lastcheck uint32

	S_checkinterval uint32

	S_creator_os uint32

	S_rev_level uint32

	S_def_resuid uint16

	S_def_resgid uint16

	S_first_ino uint32

	S_inode_size uint16

	S_block_group_nr uint16

	S_feature_compat uint32

	S_feature_incompat uint32

	S_feature_ro_compat uint32

	S_uuid [16]byte

	S_volume_name [16]byte

	S_last_mounted [64]byte

	S_algorithm_usage_bitmap uint32

	S_prealloc_blocks uint8

	S_prealloc_dir_blocks uint8

	S_reserved_gdt_blocks uint16

	S_journal_uuid [16]byte

	S_journal_inum uint32

	S_journal_dev uint32

	S_last_orphan uint32

	S_hash_seed [4]uint32

	S_def_hash_version uint8

	S_jnl_backup_type uint8

	S_desc_size uint16

	S_default_mount_opts uint32

	S_first_meta_bg uint32

	S_mkfs_time uint32

	S_jnl_blocks [17]uint32

	S_blocks_count_hi uint32

	S_r_blocks_count_hi uint32

	S_free_blocks_count_hi uint32

	S_min_extra_isize uint16

	S_want_extra_isize uint16

	S_flags uint32

	S_raid_stride uint16

	S_mmp_interval uint16

	S_mmp_block uint64

	S_raid_stripe_width uint32

	S_log_groups_per_flex uint8

	S_checksum_type uint8

	S_encryption_level uint8

	S_reserved_pad uint8

	S_kbytes_written uint64

	S_snapshot_inum uint32

	S_snapshot_id uint32

	S_snapshot_r_blocks_count uint64

	S_snapshot_list uint32

	S_error_count uint32

	S_first_error_time uint32

	S_first_error_ino uint32

	S_first_error_block uint64

	S_first_error_func [32]byte

	S_first_error_line uint32

	S_last_error_time uint32

	S_last_error_ino uint32

	S_last_error_line uint32

	S_last_error_block uint64

	S_last_error_func [32]byte

	S_mount_opts [64]byte

	S_usr_quota_inum uint32

	S_grp_quota_inum uint32

	S_overhead_blocks uint32

	S_backup_bgs [2]uint32

	S_encrypt_algos [4]uint8

	S_encrypt_pw_salt [16]byte

	S_lpf_ino uint32

	S_prj_quota_inum uint32

	S_checksum_seed uint32

	S_wtime_hi uint8

	S_mtime_hi uint8

	S_mkfs_time_hi uint8

	S_lastcheck_hi uint8

	S_first_error_time_hi uint8

	S_last_error_time_hi uint8

	S_first_error_errcode uint8

	S_last_error_errcode uint8

	S_encoding uint16

	S_encoding_flags uint16

	S_orphan_file_inum uint32

	S_reserved [94]uint32

	S_checksum uint32
}

const (

	SUPERBLOCK_OFFSET = 1024

	MAGIC_NUMBER = 0xEF53

	SUPERBLOCK_STATE_CLEAN  = 0x0001
	SUPERBLOCK_STATE_ERRORS = 0x0002
	SUPERBLOCK_STATE_ORPHAN = 0x0004

	SUPERBLOCK_ERRORS_CONTINUE = 1
	SUPERBLOCK_ERRORS_RO       = 2
	SUPERBLOCK_ERRORS_PANIC    = 3

	CREATOR_OS_LINUX   = 0
	CREATOR_OS_HURD    = 1
	CREATOR_OS_MASIX   = 2
	CREATOR_OS_FREEBSD = 3
	CREATOR_OS_LITES   = 4

	REV_LEVEL_ORIGINAL = 0
	REV_LEVEL_DYNAMIC  = 1
)

func (fs *FileSystem) ReadSuperBlock() (*SuperBlock, error) {
	var sb SuperBlock
	buf := make([]byte, 1024)

	_, err := fs.dev.ReadAt(buf, SUPERBLOCK_OFFSET)
	if err != nil {
		return nil, fmt.Errorf("failed to read superblock from device: %w", err)
	}

	err = decodeSuperBlock(buf, &sb)
	if err != nil {
		return nil, err
	}

	fs.BlockSize = sb.BlockSize()
	fs.InodeSize = sb.InodeSize()
	fs.GroupCount = sb.BlockGroupCount()
	fs.DescSize = sb.GroupDescriptorSize()

	fs.sb = &sb
	return &sb, nil
}

func decodeSuperBlock(data []byte, sb *SuperBlock) error {

	reader := bytes.NewReader(data)

	err := binary.Read(reader, binary.LittleEndian, sb)
	if err != nil {
		return fmt.Errorf("failed to parse superblock binary data: %w", err)
	}

	if sb.S_magic != MAGIC_NUMBER {
		return fmt.Errorf("invalid ext4 magic number: expected 0x%X, got 0x%X", MAGIC_NUMBER, sb.S_magic)
	}

	return nil
}

func (sb *SuperBlock) BlockSize() uint64 {
	return 1024 << sb.S_log_block_size
}

func (sb *SuperBlock) InodeSize() uint16 {
	if sb.S_rev_level > 0 {
		return sb.S_inode_size
	}
	return 128
}

func (sb *SuperBlock) BlockGroupCount() uint32 {

	groups := sb.S_blocks_count_lo / sb.S_blocks_per_group
	if sb.S_blocks_count_lo%sb.S_blocks_per_group != 0 {
		groups++
	}
	return groups
}

func (sb *SuperBlock) GroupDescriptorSize() uint16 {
	if sb.S_desc_size != 0 {
		return sb.S_desc_size
	}
	return 32
}

func (sb *SuperBlock) BlockGroupDescriptorCount() uint32 {

	return sb.BlockGroupCount()
}
