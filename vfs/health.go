package vfs

import (
	"fmt"
	"strings"
)

type FsckProblem struct {
	Severity string
	Message  string
	Fixed    bool
}

type FsckResult struct {
	InodesChecked    uint32
	DirectoriesChecked uint32
	ExtentsChecked   uint32
	BlocksReferenced uint64
	Problems         []FsckProblem
	Healthy          bool
}

func (fs *FileSystem) Fsck(fix bool) *FsckResult {
	res := &FsckResult{Healthy: true}

	if fs.sb == nil {
		res.Healthy = false
		res.Problems = append(res.Problems, FsckProblem{
			Severity: "error",
			Message:  "superblock not loaded — run ReadSuperBlock first",
		})
		return res
	}

	if fs.sb.S_magic != MAGIC_NUMBER {
		res.Healthy = false
		res.Problems = append(res.Problems, FsckProblem{
			Severity: "error",
			Message:  fmt.Sprintf("bad magic 0x%04X (expected 0x%04X)", fs.sb.S_magic, MAGIC_NUMBER),
		})
		return res
	}

	if len(fs.Bgds) == 0 {
		res.Healthy = false
		res.Problems = append(res.Problems, FsckProblem{
			Severity: "error",
			Message:  "block group descriptors not loaded — run ReadGroupDescriptors first",
		})
		return res
	}

	fs.fsckBlockBitmaps(res, fix)

	fs.fsckInodeBitmaps(res, fix)

	fs.fsckInodes(res, fix)

	fs.fsckSuperblockCounts(res, fix)

	fs.fsckDirectories(res, fix)

	return res
}

func (fs *FileSystem) fsckBlockBitmaps(res *FsckResult, fix bool) {
	for g := uint32(0); g < fs.GroupCount; g++ {
		bgd := &fs.Bgds[g]

		bitmapBlock := uint64(bgd.BG_block_bitmap_lo)
		if fs.DescSize > 32 {
			bitmapBlock |= uint64(bgd.BG_block_bitmap_hi) << 32
		}

		bitmap := make([]byte, fs.BlockSize)
		if _, err := fs.dev.ReadAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
			res.Problems = append(res.Problems, FsckProblem{
				Severity: "error",
				Message:  fmt.Sprintf("group %d: cannot read block bitmap at block %d: %v", g, bitmapBlock, err),
			})
			res.Healthy = false
			continue
		}

		blocksInGroup := fs.sb.S_blocks_per_group
		bitsToCheck := int(blocksInGroup)
		if bitsToCheck > len(bitmap)*8 {
			bitsToCheck = len(bitmap) * 8
		}
		set := uint16(0)
		for i := 0; i < bitsToCheck; i++ {
			if bitmap[i/8]&(1<<(i%8)) != 0 {
				set++
			}
		}

		declared := bgd.BG_free_blocks_count_lo
		total := uint64(fs.sb.S_blocks_per_group)
		used := uint32(total) - uint32(set)
		expectedFree := uint32(total) - used
		if uint32(declared) != expectedFree && expectedFree <= uint32(total) {
			msg := fmt.Sprintf("group %d: block bitmap shows %d used blocks but descriptor claims %d free (expected %d)",
				g, set, declared, expectedFree)
			if fix && expectedFree <= uint32(total) {
				bgd.BG_free_blocks_count_lo = uint16(expectedFree)
				if err := fs.WriteGroupDescriptor(g, bgd); err == nil {
					msg += " — FIXED"
					res.Problems = append(res.Problems, FsckProblem{Severity: "warning", Message: msg, Fixed: true})
					continue
				}
			}
			res.Problems = append(res.Problems, FsckProblem{Severity: "error", Message: msg})
			res.Healthy = false
		}
	}
}

func (fs *FileSystem) fsckInodeBitmaps(res *FsckResult, fix bool) {
	for g := uint32(0); g < fs.GroupCount; g++ {
		bgd := &fs.Bgds[g]

		bitmapBlock := uint64(bgd.BG_inode_bitmap_lo)
		if fs.DescSize > 32 {
			bitmapBlock |= uint64(bgd.BG_inode_bitmap_hi) << 32
		}

		bitmap := make([]byte, fs.BlockSize)
		if _, err := fs.dev.ReadAt(bitmap, int64(bitmapBlock*fs.BlockSize)); err != nil {
			res.Problems = append(res.Problems, FsckProblem{
				Severity: "error",
				Message:  fmt.Sprintf("group %d: cannot read inode bitmap at block %d: %v", g, bitmapBlock, err),
			})
			res.Healthy = false
			continue
		}

		inodesInGroup := fs.sb.S_inodes_per_group
		bitsToCheck := int(inodesInGroup)
		if bitsToCheck > len(bitmap)*8 {
			bitsToCheck = len(bitmap) * 8
		}
		set := uint16(0)
		for i := 0; i < bitsToCheck; i++ {
			if bitmap[i/8]&(1<<(i%8)) != 0 {
				set++
			}
		}

		declared := bgd.BG_free_inodes_count_lo
		used := uint32(bitsToCheck) - uint32(set)
		expectedFree := uint32(bitsToCheck) - used
		if uint32(declared) != expectedFree && expectedFree <= inodesInGroup {
			msg := fmt.Sprintf("group %d: inode bitmap shows %d used inodes but descriptor claims %d free (expected %d)",
				g, set, declared, expectedFree)
			if fix && expectedFree <= inodesInGroup {
				bgd.BG_free_inodes_count_lo = uint16(expectedFree)
				if err := fs.WriteGroupDescriptor(g, bgd); err == nil {
					msg += " — FIXED"
					res.Problems = append(res.Problems, FsckProblem{Severity: "warning", Message: msg, Fixed: true})
					continue
				}
			}
			res.Problems = append(res.Problems, FsckProblem{Severity: "error", Message: msg})
			res.Healthy = false
		}
	}
}

func (fs *FileSystem) fsckInodes(res *FsckResult, fix bool) {
	totalInodes := fs.sb.S_inodes_count

	maxCheck := uint32(1000)
	if totalInodes < maxCheck {
		maxCheck = totalInodes
	}

	for inum := uint32(1); inum <= maxCheck && inum <= totalInodes; inum++ {
		inode, err := fs.ReadInode(inum)
		if err != nil {

			if strings.Contains(err.Error(), "group") && strings.Contains(err.Error(), "out of range") {
				break
			}
			continue
		}
		res.InodesChecked++

		fileType := inode.I_mode & ifmt
		switch fileType {
		case S_IFREG, S_IFDIR, S_IFLNK, S_IFCHR, S_IFBLK, S_IFIFO, S_IFSOCK:

		default:
			if inode.I_mode != 0 {
				res.Problems = append(res.Problems, FsckProblem{
					Severity: "warning",
					Message:  fmt.Sprintf("inode %d: unknown file type 0x%04X", inum, fileType),
				})
			}
			continue
		}

		if fileType != S_IFREG && fileType != S_IFDIR && fileType != S_IFLNK {
			continue
		}

		if !inode.UsesExtents() {
			continue
		}

		extents, err := fs.ReadExtents(inode)
		if err != nil {
			if inode.I_mode != 0 {
				res.Problems = append(res.Problems, FsckProblem{
					Severity: "error",
					Message:  fmt.Sprintf("inode %d: bad extent tree: %v", inum, err),
				})
				res.Healthy = false
			}
			continue
		}
		res.ExtentsChecked += uint32(len(extents))

		totalBlocks := uint64(0)
		for _, ext := range extents {
			blockCount := uint64(ext.EE_len & 0x7FFF)
			totalBlocks += blockCount
			physStart := (uint64(ext.EE_start_hi) << 32) | uint64(ext.EE_start_lo)
			res.BlocksReferenced += blockCount

			if physStart+blockCount > uint64(fs.sb.S_blocks_count_lo) {
				res.Problems = append(res.Problems, FsckProblem{
					Severity: "error",
					Message:  fmt.Sprintf("inode %d: extent [%d..%d) exceeds filesystem block count %d",
						inum, physStart, physStart+blockCount, fs.sb.S_blocks_count_lo),
				})
				res.Healthy = false
			}
		}

		if fileType == S_IFREG || fileType == S_IFDIR {
			fileSize := inode.Size()
			minBlocks := (fileSize + fs.BlockSize - 1) / fs.BlockSize
			if totalBlocks < minBlocks {
				res.Problems = append(res.Problems, FsckProblem{
					Severity: "warning",
					Message:  fmt.Sprintf("inode %d: file size %d but only %d blocks allocated (need %d)",
						inum, fileSize, totalBlocks, minBlocks),
				})
			}
		}
	}
}

func (fs *FileSystem) fsckSuperblockCounts(res *FsckResult, fix bool) {
	totalFreeBlocks := uint64(0)
	totalFreeInodes := uint64(0)
	for _, bgd := range fs.Bgds {
		totalFreeBlocks += uint64(bgd.BG_free_blocks_count_lo)
		totalFreeInodes += uint64(bgd.BG_free_inodes_count_lo)
	}

	sbFreeBlocks := uint64(fs.sb.S_free_blocks_count_lo)
	sbFreeInodes := uint64(fs.sb.S_free_inodes_count)

	if totalFreeBlocks != sbFreeBlocks {
		msg := fmt.Sprintf("superblock: free blocks = %d but group sum = %d", sbFreeBlocks, totalFreeBlocks)
		if fix {
			fs.sb.S_free_blocks_count_lo = uint32(totalFreeBlocks)
			fs.writeSuperBlock()
			msg += " — FIXED"
			res.Problems = append(res.Problems, FsckProblem{Severity: "warning", Message: msg, Fixed: true})
		} else {
			res.Problems = append(res.Problems, FsckProblem{Severity: "error", Message: msg})
			res.Healthy = false
		}
	}

	if totalFreeInodes != sbFreeInodes {
		msg := fmt.Sprintf("superblock: free inodes = %d but group sum = %d", sbFreeInodes, totalFreeInodes)
		if fix {
			fs.sb.S_free_inodes_count = uint32(totalFreeInodes)
			fs.writeSuperBlock()
			msg += " — FIXED"
			res.Problems = append(res.Problems, FsckProblem{Severity: "warning", Message: msg, Fixed: true})
		} else {
			res.Problems = append(res.Problems, FsckProblem{Severity: "error", Message: msg})
			res.Healthy = false
		}
	}
}

func (fs *FileSystem) fsckDirectories(res *FsckResult, fix bool) {
	totalInodes := fs.sb.S_inodes_count
	maxCheck := uint32(1000)
	if totalInodes < maxCheck {
		maxCheck = totalInodes
	}

	for inum := uint32(1); inum <= maxCheck && inum <= totalInodes; inum++ {
		inode, err := fs.ReadInode(inum)
		if err != nil {
			if strings.Contains(err.Error(), "group") {
				break
			}
			continue
		}
		if !inode.IsDir() {
			continue
		}
		res.DirectoriesChecked++

		entries, err := fs.ReadDir(inode)
		if err != nil {
			res.Problems = append(res.Problems, FsckProblem{
				Severity: "error",
				Message:  fmt.Sprintf("directory inode %d: cannot read: %v", inum, err),
			})
			res.Healthy = false
			continue
		}

		seen := make(map[string]bool)
		for _, e := range entries {
			if e.Inode == 0 {
				continue
			}
			if e.Inode > totalInodes {
				res.Problems = append(res.Problems, FsckProblem{
					Severity: "error",
					Message:  fmt.Sprintf("dir %d: entry %q points to inode %d which exceeds total inodes %d",
						inum, e.Name, e.Inode, totalInodes),
				})
				res.Healthy = false
			}
			if seen[e.Name] {
				res.Problems = append(res.Problems, FsckProblem{
					Severity: "error",
					Message:  fmt.Sprintf("dir %d: duplicate entry %q", inum, e.Name),
				})
				res.Healthy = false
			}
			seen[e.Name] = true
		}

		if !seen["."] || !seen[".."] {
			res.Problems = append(res.Problems, FsckProblem{
				Severity: "error",
				Message:  fmt.Sprintf("directory inode %d: missing . or .. entry", inum),
			})
			res.Healthy = false
		}
	}
}

func (r *FsckResult) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Inodes checked:  %d\n", r.InodesChecked)
	fmt.Fprintf(&b, "Directories:     %d\n", r.DirectoriesChecked)
	fmt.Fprintf(&b, "Extents checked: %d\n", r.ExtentsChecked)
	fmt.Fprintf(&b, "Blocks ref'd:    %d\n", r.BlocksReferenced)
	if len(r.Problems) == 0 {
		fmt.Fprintf(&b, "\n✓ No problems found — filesystem is healthy")
	} else {
		errors := 0
		warnings := 0
		fixed := 0
		for _, p := range r.Problems {
			if p.Severity == "error" {
				errors++
			} else {
				warnings++
			}
			if p.Fixed {
				fixed++
			}
		}
		fmt.Fprintf(&b, "\nProblems: %d errors, %d warnings (%d fixed)\n", errors, warnings, fixed)
		for _, p := range r.Problems {
			tag := p.Severity
			if p.Fixed {
				tag = "fixed"
			}
			fmt.Fprintf(&b, "  [%s] %s\n", tag, p.Message)
		}
		if r.Healthy {
			fmt.Fprintf(&b, "\n✓ Overall: healthy after repairs")
		} else {
			fmt.Fprintf(&b, "\n✗ Overall: unhealthy — run `werunos fsck --fix <device>` to repair")
		}
	}
	return b.String()
}
