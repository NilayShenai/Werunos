package vfs

import (
	"encoding/binary"
	"fmt"
	"log"
)

type RecoveryResult struct {
	JournalReplayed    bool
	BlocksApplied      uint32
	Transactions       uint32
	OrphansCleaned     uint32
	JournalSkipped     string
}

func (fs *FileSystem) Recover() *RecoveryResult {
	res := &RecoveryResult{}

	if fs.JournalNeedsRecovery() {
		res.JournalReplayed = true
		err := fs.replayJournal(res)
		if err != nil {
			res.JournalSkipped = fmt.Sprintf("journal replay failed: %v", err)
			log.Printf("[ext4] %s — mounting anyway", res.JournalSkipped)
		}
	}

	if err := fs.cleanOrphans(res); err != nil {
		log.Printf("[ext4] orphan cleanup failed: %v", err)
	}

	fs.sb.S_feature_incompat &^= EXT4_FEATURE_INCOMPAT_RECOVERY
	fs.writeSuperBlock()

	return res
}

const (
	JBD2_MAGIC       = 0xC03B3998
	JBD2_MAGIC_OLD   = 0xC03B3999
	JBD2_ESC_MAGIC   = 0x4A6F6E75

	JBD2_DESCRIPTOR  = 1
	JBD2_BLOCK       = 2
	JBD2_COMMIT      = 3
	JBD2_REVOKE      = 4
	JBD2_SUPERBLOCK  = 5

	JBD2_FLAG_ESCAPE     = 1
	JBD2_FLAG_SAME_UUID  = 4
	JBD2_FLAG_LAST_TAG   = 64
	JBD2_FLAG_CRC32      = 128

	EXT4_FEATURE_INCOMPAT_RECOVERY = 0x0001
)

type jbd2Header struct {
	H_magic     uint32
	H_blocktype uint32
	H_sequence  uint32
}

type jbd2Superblock struct {
	Header         jbd2Header
	S_sequence     uint32
	S_start        uint32
	S_maxlen       uint32
}

type jbd2Tag struct {
	T_blocknr uint32
	T_flags   uint32
}

func (fs *FileSystem) JournalNeedsRecovery() bool {
	if fs.sb == nil {
		return false
	}
	return fs.sb.S_feature_incompat&EXT4_FEATURE_INCOMPAT_RECOVERY != 0
}

func (fs *FileSystem) replayJournal(res *RecoveryResult) error {
	journalInum := fs.sb.S_journal_inum
	if journalInum == 0 {
		return fmt.Errorf("S_journal_inum is 0")
	}
	if fs.sb.S_journal_dev != 0 {
		return fmt.Errorf("external journal (dev=%d) not supported", fs.sb.S_journal_dev)
	}

	jinode, err := fs.ReadInode(journalInum)
	if err != nil {
		return fmt.Errorf("failed to read journal inode %d: %w", journalInum, err)
	}

	extents, err := fs.ReadExtents(jinode)
	if err != nil {
		return fmt.Errorf("failed to read journal extents: %w", err)
	}

	blockSize := int(fs.BlockSize)

	jsbBlock, found := findPhysicalBlock(extents, 0)
	if !found {
		return fmt.Errorf("journal block 0 not found")
	}
	jsbBuf := make([]byte, blockSize)
	if _, err := fs.dev.ReadAt(jsbBuf, int64(jsbBlock*fs.BlockSize)); err != nil {
		return fmt.Errorf("failed to read journal superblock: %w", err)
	}
	var jsb jbd2Superblock
	jsb.Header.H_magic = binary.LittleEndian.Uint32(jsbBuf[0:4])
	jsb.Header.H_blocktype = binary.LittleEndian.Uint32(jsbBuf[4:8])
	jsb.Header.H_sequence = binary.LittleEndian.Uint32(jsbBuf[8:12])
	jsb.S_sequence = binary.LittleEndian.Uint32(jsbBuf[12:16])
	jsb.S_start = binary.LittleEndian.Uint32(jsbBuf[16:20])
	jsb.S_maxlen = binary.LittleEndian.Uint32(jsbBuf[20:24])

	if jsb.Header.H_magic != JBD2_MAGIC && jsb.Header.H_magic != JBD2_MAGIC_OLD {
		return fmt.Errorf("bad journal magic 0x%08X", jsb.Header.H_magic)
	}

	if jsb.S_start == 0 || jsb.S_start > jsb.S_maxlen {
		log.Printf("[ext4] journal is empty")
		return nil
	}

	pos := jsb.S_start

	for {
		if pos > jsb.S_maxlen {
			pos = 1
		}
		if pos == jsb.S_start && res.Transactions > 0 {
			break
		}

		hdr, buf, err := fs.readJournalBlock(extents, pos)
		if err != nil {
			return fmt.Errorf("block %d: %w", pos, err)
		}

		skipAdvance := false

		switch hdr.H_blocktype {
		case JBD2_SUPERBLOCK:
			goto done

		case JBD2_DESCRIPTOR:
			skipAdvance = true
			off := 12
			tagSize := 8
			for off+tagSize <= blockSize {
				tagBlocknr := binary.LittleEndian.Uint32(buf[off : off+4])
				tagFlags := binary.LittleEndian.Uint32(buf[off+4 : off+8])
				off += tagSize

				if tagFlags&JBD2_FLAG_CRC32 != 0 {
					off += 4
				}
				if tagBlocknr == 0 && tagFlags == 0 {
					break
				}

				pos++
				if pos > jsb.S_maxlen {
					pos = 1
				}
				if pos == jsb.S_start {
					return fmt.Errorf("unexpected wrap during descriptor data")
				}

				dataHdr, dataBuf, err := fs.readJournalBlock(extents, pos)
				if err != nil {
					return fmt.Errorf("data for block %d: %w", tagBlocknr, err)
				}
				if dataHdr.H_blocktype != JBD2_BLOCK {
					return fmt.Errorf("expected data block at pos %d, got type %d", pos, dataHdr.H_blocktype)
				}

				if tagFlags&JBD2_FLAG_ESCAPE != 0 {
					escBytes := [4]byte{0x4A, 0x6F, 0x6E, 0x75}
					for i := 0; i < 4 && i < len(dataBuf); i++ {
						dataBuf[i] ^= escBytes[i]
					}
				}

				absBlock := uint64(tagBlocknr)
				targetOff := int64(absBlock * fs.BlockSize)
				if _, err := fs.dev.WriteAt(dataBuf, targetOff); err != nil {
					return fmt.Errorf("write to block %d: %w", absBlock, err)
				}
				res.BlocksApplied++

				if tagFlags&JBD2_FLAG_LAST_TAG != 0 {
					break
				}
			}

		case JBD2_COMMIT:
			res.Transactions++

		case JBD2_REVOKE:
		}

		if !skipAdvance {
			pos++
		}
		if pos > jsb.S_maxlen {
			pos = 1
		}
	}

done:
	log.Printf("[ext4] journal: %d blocks applied, %d transactions", res.BlocksApplied, res.Transactions)
	return nil
}

func (fs *FileSystem) cleanOrphans(res *RecoveryResult) error {
	if fs.sb.S_last_orphan == 0 {
		return nil
	}

	log.Printf("[ext4] cleaning orphans (head inode %d)...", fs.sb.S_last_orphan)

	next := fs.sb.S_last_orphan
	for next != 0 {
		orphanNum := next
		inode, err := fs.ReadInode(orphanNum)
		if err != nil {
			return fmt.Errorf("failed to read orphan inode %d: %w", orphanNum, err)
		}

		next = inode.I_dtime

		if inode.UsesExtents() {
			if extents, err := fs.ReadExtents(inode); err == nil {
				for _, ext := range extents {
					if ext.EE_len&0x8000 != 0 {
						continue
					}
					blockCount := uint64(ext.EE_len & 0x7FFF)
					physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
					for i := uint64(0); i < blockCount; i++ {
						fs.FreeBlock(physStart + i)
					}
				}
			}
		}

		if err := fs.FreeInode(orphanNum); err != nil {
			log.Printf("[ext4] WARNING: failed to free orphan inode %d: %v", orphanNum, err)
		}
		res.OrphansCleaned++
	}

	fs.sb.S_last_orphan = 0
	fs.writeSuperBlock()
	log.Printf("[ext4] orphan cleanup: %d inodes freed", res.OrphansCleaned)
	return nil
}

func (fs *FileSystem) readJournalBlock(extents []Extent, logicalBlock uint32) (jbd2Header, []byte, error) {
	var hdr jbd2Header
	physBlock, found := findPhysicalBlock(extents, logicalBlock)
	if !found {
		return hdr, nil, fmt.Errorf("journal logical block %d not found in extents", logicalBlock)
	}

	blockSize := int(fs.BlockSize)
	buf := make([]byte, blockSize)
	if _, err := fs.dev.ReadAt(buf, int64(physBlock*fs.BlockSize)); err != nil {
		return hdr, nil, fmt.Errorf("failed to read journal block %d (phys %d): %w", logicalBlock, physBlock, err)
	}

	hdr.H_magic = binary.LittleEndian.Uint32(buf[0:4])
	hdr.H_blocktype = binary.LittleEndian.Uint32(buf[4:8])
	hdr.H_sequence = binary.LittleEndian.Uint32(buf[8:12])
	return hdr, buf, nil
}

func (fs *FileSystem) clearRecoveryFlag() {
	if fs.sb == nil {
		return
	}
	fs.sb.S_feature_incompat &^= EXT4_FEATURE_INCOMPAT_RECOVERY
	fs.writeSuperBlock()
}

func (fs *FileSystem) EnsureRecoveryFlagIsClear() {
	if fs.JournalNeedsRecovery() {
		log.Printf("[ext4] RECOVERY flag still set — clearing")
		fs.clearRecoveryFlag()
	}
}
