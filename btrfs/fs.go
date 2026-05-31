package btrfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/NilayShenai/Werunos/fs"
	"github.com/winfsp/cgofuse/fuse"
)

var _ = binary.LittleEndian

type readWriterAt interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
}

type FileSystem struct {
	dev           readWriterAt
	sb            *SuperBlock
	tc            *treeContext
	fsRoot        uint64
	fsRootLvl     uint8
	extentRoot    uint64
	extentRootLvl uint8
	rootInode     uint64
	pathCache     sync.Map
	nextInode     uint64
	mu            sync.Mutex
}

func New(dev interface {
	ReadAt(p []byte, off int64) (int, error)
	WriteAt(p []byte, off int64) (int, error)
}) *FileSystem {
	return &FileSystem{dev: dev}
}

func (b *FileSystem) Type() string { return "btrfs" }

func (b *FileSystem) ReadSuperBlock() error {
	sb, err := ReadSuperBlock(b.dev)
	if err != nil {
		return err
	}
	b.sb = sb

	tc := &treeContext{dev: b.dev, nodeSize: sb.NodeSize}
	if err := sb.parseSysChunks(tc); err != nil {
		return err
	}
	if len(tc.chunks) > 0 {
		if err := tc.resolveChunkTree(sb.ChunkRoot, sb.ChunkRootLevel); err != nil {
			return err
		}
	}
	b.tc = tc

	fsRoot, fsLvl, rootDirID, err := b.findFSTreeRoot()
	if err != nil {
		return err
	}
	b.fsRoot = fsRoot
	b.fsRootLvl = fsLvl
	b.rootInode = rootDirID

	extRoot, extLvl, err := b.findExtentTreeRoot()
	if err == nil {
		b.extentRoot = extRoot
		b.extentRootLvl = extLvl
	}

	return nil
}

func (b *FileSystem) BlockSize() uint64 { return uint64(b.sb.SectorSize) }

func (b *FileSystem) Destroy() {}

func (b *FileSystem) Statfs() *fuse.Statfs_t {
	total := b.sb.TotalBytes
	used := b.sb.BytesUsed
	return &fuse.Statfs_t{
		Bsize:   uint64(b.sb.SectorSize),
		Frsize:  uint64(b.sb.SectorSize),
		Blocks:  total / uint64(b.sb.SectorSize),
		Bfree:   (total - used) / uint64(b.sb.SectorSize),
		Bavail:  (total - used) / uint64(b.sb.SectorSize),
		Files:   0,
		Ffree:   0,
		Favail:  0,
		Namemax: 255,
	}
}

func (b *FileSystem) Getattr(path string) (fs.NodeInfo, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return fs.NodeInfo{}, err
	}
	return b.readInodeInfo(inodeNum)
}

func (b *FileSystem) Readdir(path string) ([]fs.DirEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return nil, err
	}
	return b.readDirEntries(inodeNum)
}

func (b *FileSystem) Readlink(path string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return "", err
	}
	return b.readSymlink(inodeNum)
}

func (b *FileSystem) Open(path string, flags int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, err := b.resolvePath(path)
	return err
}

func (b *FileSystem) Read(path string, buf []byte, ofst int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return 0, err
	}
	return b.readFile(inodeNum, buf, ofst)
}

func (b *FileSystem) Release(path string) error { return nil }

func (b *FileSystem) Create(path string, flags int, mode uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.createFile(path, mode|0x8000, BTRFS_FT_REG_FILE)
}

func (b *FileSystem) Write(path string, buf []byte, ofst int64) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(buf) == 0 {
		return 0, nil
	}
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return 0, err
	}

	var extData []byte
	var foundExtKey key
	b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == inodeNum && item.key.typ == BTRFS_EXTENT_DATA_KEY {
			extKey := item.key
			if extKey.offset == 0 {
				foundExtKey = extKey
				extData = make([]byte, len(d))
				copy(extData, d)
			}
		}
		return nil
	})

	var currentContent []byte
	if len(extData) > 0 {
		extType := extData[20]
		if extType == BTRFS_FILE_EXTENT_INLINE {
			r := &byteReader{buf: extData}
			r.skip(8)
			ramBytes := r.u64()
			ctype := r.u8()
			r.skip(1)
			r.skip(2)
			r.skip(1)
			raw := extData[r.off:]
			if ctype != BTRFS_COMPRESS_NONE {
				dec, err := decompressData(ctype, raw, ramBytes)
				if err == nil {
					currentContent = dec
				}
			} else {
				currentContent = make([]byte, len(raw))
				copy(currentContent, raw)
			}
		} else if extType == BTRFS_FILE_EXTENT_REG {
			diskBytenr := binary.LittleEndian.Uint64(extData[21:29])
			numBytes := binary.LittleEndian.Uint64(extData[45:53])
			if diskBytenr > 0 && numBytes > 0 {
				phys, err := b.tc.resolve(diskBytenr)
				if err == nil {
					currentContent = make([]byte, numBytes)
					_, _ = b.dev.ReadAt(currentContent, int64(phys))
				}
			}
		}
	}

	newSize := uint64(ofst) + uint64(len(buf))
	finalContent := make([]byte, newSize)
	if len(currentContent) > 0 {
		copy(finalContent, currentContent)
	}
	copy(finalContent[ofst:], buf)

	if newSize <= 2048 {
		newExtItem := makeExtentDataInline(0, finalContent)
		if len(extData) > 0 {
			if err := b.updateInLeaf(b.fsRoot, foundExtKey, newExtItem); err != nil {
				return 0, err
			}
		} else {
			if err := b.insertIntoLeaf(b.fsRoot, key{
				objectid: inodeNum,
				typ:      BTRFS_EXTENT_DATA_KEY,
				offset:   0,
			}, newExtItem); err != nil {
				return 0, err
			}
		}
	} else {
		sectorSize := uint64(b.sb.SectorSize)
		if sectorSize == 0 {
			sectorSize = 4096
		}

		var payload []byte
		var compression uint8
		var writeSize uint64

		if compressed, cerr := compressZlib(finalContent); cerr == nil && len(compressed) < len(finalContent) {
			payload = compressed
			compression = BTRFS_COMPRESS_ZLIB
			writeSize = uint64(len(compressed))
		} else {
			payload = finalContent
			compression = BTRFS_COMPRESS_NONE
			writeSize = newSize
		}

		alignedSize := ((writeSize + sectorSize - 1) / sectorSize) * sectorSize

		logicalAddr, physAddr, err := b.allocateSpace(alignedSize, BTRFS_BLOCK_GROUP_DATA)
		if err != nil {
			return 0, err
		}

		diskBuf := make([]byte, alignedSize)
		copy(diskBuf, payload)

		if _, err := b.dev.WriteAt(diskBuf, int64(physAddr)); err != nil {
			return 0, fmt.Errorf("btrfs: write regular extent: %w", err)
		}

		_ = b.registerExtent(logicalAddr, alignedSize)

		var keysToDelete []key
		b.walkFSTree(func(item leafItem, d []byte) error {
			if item.key.objectid == inodeNum && item.key.typ == BTRFS_EXTENT_DATA_KEY {
				keysToDelete = append(keysToDelete, item.key)
			}
			return nil
		})
		for _, k := range keysToDelete {
			_ = b.deleteFromLeaf(b.fsRoot, k)
		}

		newExtItem := b.makeExtentDataRegularCompressed(logicalAddr, alignedSize, newSize, compression)
		if err := b.insertIntoLeaf(b.fsRoot, key{
			objectid: inodeNum,
			typ:      BTRFS_EXTENT_DATA_KEY,
			offset:   0,
		}, newExtItem); err != nil {
			return 0, err
		}
	}

	var inodeData []byte
	b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == inodeNum && item.key.typ == BTRFS_INODE_ITEM_KEY {
			inodeData = make([]byte, len(d))
			copy(inodeData, d)
		}
		return nil
	})
	if len(inodeData) >= 100 {
		oldSize := binary.LittleEndian.Uint64(inodeData[16:24])
		if newSize > oldSize {
			binary.LittleEndian.PutUint64(inodeData[16:24], newSize)
		}
		now := uint64(time.Now().Unix())
		binary.LittleEndian.PutUint64(inodeData[120:128], now)
		binary.LittleEndian.PutUint32(inodeData[128:132], 0)
		binary.LittleEndian.PutUint64(inodeData[132:140], now)
		binary.LittleEndian.PutUint32(inodeData[140:144], 0)
		b.updateInLeaf(b.fsRoot, key{
			objectid: inodeNum,
			typ:      BTRFS_INODE_ITEM_KEY,
			offset:   0,
		}, inodeData)
	}

	b.pathCache.Delete(path)
	return len(buf), nil
}

func (b *FileSystem) makeExtentDataRegular(logicalAddr uint64, diskSize uint64, ramSize uint64) []byte {
	return b.makeExtentDataRegularCompressed(logicalAddr, diskSize, ramSize, BTRFS_COMPRESS_NONE)
}

func (b *FileSystem) makeExtentDataRegularCompressed(logicalAddr uint64, diskSize uint64, ramSize uint64, compression uint8) []byte {
	buf := make([]byte, 53)
	binary.LittleEndian.PutUint64(buf[0:8], b.sb.Generation)
	binary.LittleEndian.PutUint64(buf[8:16], ramSize)
	buf[16] = compression
	buf[17] = 0
	binary.LittleEndian.PutUint16(buf[18:20], 0)
	buf[20] = BTRFS_FILE_EXTENT_REG
	binary.LittleEndian.PutUint64(buf[21:29], logicalAddr)
	binary.LittleEndian.PutUint64(buf[29:37], diskSize)
	binary.LittleEndian.PutUint64(buf[37:45], 0)
	binary.LittleEndian.PutUint64(buf[45:53], ramSize)
	return buf
}

func (b *FileSystem) Mkdir(path string, mode uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.createFile(path, mode|0x4000, BTRFS_FT_DIR)
}

func (b *FileSystem) Unlink(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.removeEntry(path, false)
}

func (b *FileSystem) Rmdir(path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.removeEntry(path, true)
}

func (b *FileSystem) removeEntry(path string, mustBeDir bool) error {
	dir, name, err := splitPath(path)
	if err != nil {
		return err
	}

	dirInode, err := b.resolvePath(dir)
	if err != nil {
		return err
	}

	entryKey, childInodeNum, err := b.lookupDirEntryKey(dirInode, name)
	if err != nil {
		return err
	}

	childInfo, err := b.readInodeInfo(childInodeNum)
	if err != nil {
		return err
	}

	if mustBeDir && !childInfo.IsDir {
		return fmt.Errorf("btrfs: not a directory")
	}
	if !mustBeDir && childInfo.IsDir {
		return fmt.Errorf("btrfs: is a directory")
	}

	if childInfo.IsDir {
		entries, err := b.readDirEntries(childInodeNum)
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("btrfs: directory not empty")
		}
	}

	if err := b.deleteFromLeaf(b.fsRoot, entryKey); err != nil {
		return err
	}

	var inodeData []byte
	err = b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == childInodeNum && item.key.typ == BTRFS_INODE_ITEM_KEY {
			inodeData = make([]byte, len(d))
			copy(inodeData, d)
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return err
	}

	if len(inodeData) >= 100 {
		nlink := binary.LittleEndian.Uint32(inodeData[40:44])
		if nlink > 0 {
			nlink--
		}
		binary.LittleEndian.PutUint32(inodeData[40:44], nlink)
		now := uint64(time.Now().Unix())
		binary.LittleEndian.PutUint64(inodeData[120:128], now)
		binary.LittleEndian.PutUint32(inodeData[128:132], 0)

		if nlink == 0 {

			if err := b.deleteFromLeaf(b.fsRoot, key{
				objectid: childInodeNum,
				typ:      BTRFS_INODE_ITEM_KEY,
				offset:   0,
			}); err != nil {
				return err
			}

			var extentKeys []key
			b.walkFSTree(func(item leafItem, d []byte) error {
				if item.key.objectid == childInodeNum && item.key.typ == BTRFS_EXTENT_DATA_KEY {
					extentKeys = append(extentKeys, item.key)
				}
				return nil
			})
			for _, ek := range extentKeys {
				b.deleteFromLeaf(b.fsRoot, ek)
			}
		} else {

			if err := b.updateInLeaf(b.fsRoot, key{
				objectid: childInodeNum,
				typ:      BTRFS_INODE_ITEM_KEY,
				offset:   0,
			}, inodeData); err != nil {
				return err
			}
		}
	}

	b.pathCache.Delete(path)
	b.pathCache.Delete(dir)
	return nil
}

func (b *FileSystem) Rename(oldpath, newpath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	oldDir, oldName, err := splitPath(oldpath)
	if err != nil {
		return err
	}
	newDir, newName, err := splitPath(newpath)
	if err != nil {
		return err
	}

	oldDirInode, err := b.resolvePath(oldDir)
	if err != nil {
		return err
	}
	newDirInode, err := b.resolvePath(newDir)
	if err != nil {
		return err
	}

	entryKey, childInodeNum, err := b.lookupDirEntryKey(oldDirInode, oldName)
	if err != nil {
		return err
	}

	childInfo, err := b.readInodeInfo(childInodeNum)
	if err != nil {
		return err
	}
	fileType := uint8(BTRFS_FT_REG_FILE)
	if childInfo.IsDir {
		fileType = BTRFS_FT_DIR
	} else if childInfo.IsSymlink {
		fileType = BTRFS_FT_SYMLINK
	}

	if targetKey, targetInode, err := b.lookupDirEntryKey(newDirInode, newName); err == nil {

		if err := b.deleteFromLeaf(b.fsRoot, targetKey); err != nil {
			return err
		}

		var targetData []byte
		b.walkFSTree(func(item leafItem, d []byte) error {
			if item.key.objectid == targetInode && item.key.typ == BTRFS_INODE_ITEM_KEY {
				targetData = make([]byte, len(d))
				copy(targetData, d)
			}
			return nil
		})
		if len(targetData) >= 100 {
			nlink := binary.LittleEndian.Uint32(targetData[40:44])
			if nlink > 0 {
				nlink--
			}
			binary.LittleEndian.PutUint32(targetData[40:44], nlink)
			if nlink == 0 {
				b.deleteFromLeaf(b.fsRoot, key{objectid: targetInode, typ: BTRFS_INODE_ITEM_KEY, offset: 0})

				var extentKeys []key
				b.walkFSTree(func(item leafItem, d []byte) error {
					if item.key.objectid == targetInode && item.key.typ == BTRFS_EXTENT_DATA_KEY {
						extentKeys = append(extentKeys, item.key)
					}
					return nil
				})
				for _, ek := range extentKeys {
					b.deleteFromLeaf(b.fsRoot, ek)
				}
			} else {
				b.updateInLeaf(b.fsRoot, key{objectid: targetInode, typ: BTRFS_INODE_ITEM_KEY, offset: 0}, targetData)
			}
		}
	}

	if err := b.deleteFromLeaf(b.fsRoot, entryKey); err != nil {
		return err
	}

	nextOff := b.nextDirIndex(newDirInode)
	dirData := makeDirItem(childInodeNum, newName, fileType)
	if err := b.insertIntoLeaf(b.fsRoot, key{
		objectid: newDirInode,
		typ:      BTRFS_DIR_INDEX_KEY,
		offset:   nextOff,
	}, dirData); err != nil {
		return err
	}

	now := uint64(time.Now().Unix())
	for _, dirNode := range []uint64{oldDirInode, newDirInode} {
		var dData []byte
		b.walkFSTree(func(item leafItem, d []byte) error {
			if item.key.objectid == dirNode && item.key.typ == BTRFS_INODE_ITEM_KEY {
				dData = make([]byte, len(d))
				copy(dData, d)
			}
			return nil
		})
		if len(dData) >= 100 {
			binary.LittleEndian.PutUint64(dData[120:128], now)
			binary.LittleEndian.PutUint32(dData[128:132], 0)
			binary.LittleEndian.PutUint64(dData[132:140], now)
			binary.LittleEndian.PutUint32(dData[140:144], 0)
			b.updateInLeaf(b.fsRoot, key{objectid: dirNode, typ: BTRFS_INODE_ITEM_KEY, offset: 0}, dData)
		}
	}

	b.pathCache.Delete(oldpath)
	b.pathCache.Delete(newpath)
	b.pathCache.Delete(oldDir)
	b.pathCache.Delete(newDir)
	return nil
}

func (b *FileSystem) Chmod(path string, mode uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return err
	}

	var data []byte
	err = b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == inodeNum && item.key.typ == BTRFS_INODE_ITEM_KEY {
			data = make([]byte, len(d))
			copy(data, d)
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return err
	}
	if len(data) < 100 {
		return fmt.Errorf("btrfs: inode %d not found or invalid", inodeNum)
	}

	binary.LittleEndian.PutUint32(data[52:56], (binary.LittleEndian.Uint32(data[52:56])&0xFFFF0000)|(mode&0x0FFF))

	now := uint64(time.Now().Unix())
	binary.LittleEndian.PutUint64(data[120:128], now)
	binary.LittleEndian.PutUint32(data[128:132], 0)

	return b.updateInLeaf(b.fsRoot, key{
		objectid: inodeNum,
		typ:      BTRFS_INODE_ITEM_KEY,
		offset:   0,
	}, data)
}

func (b *FileSystem) Chown(path string, uid, gid uint32) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return err
	}

	var data []byte
	err = b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == inodeNum && item.key.typ == BTRFS_INODE_ITEM_KEY {
			data = make([]byte, len(d))
			copy(data, d)
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return err
	}
	if len(data) < 100 {
		return fmt.Errorf("btrfs: inode %d not found or invalid", inodeNum)
	}

	if uid != ^uint32(0) {
		binary.LittleEndian.PutUint32(data[44:48], uid)
	}
	if gid != ^uint32(0) {
		binary.LittleEndian.PutUint32(data[48:52], gid)
	}

	now := uint64(time.Now().Unix())
	binary.LittleEndian.PutUint64(data[120:128], now)
	binary.LittleEndian.PutUint32(data[128:132], 0)

	return b.updateInLeaf(b.fsRoot, key{
		objectid: inodeNum,
		typ:      BTRFS_INODE_ITEM_KEY,
		offset:   0,
	}, data)
}

func (b *FileSystem) Truncate(path string, size int64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	inodeNum, err := b.resolvePath(path)
	if err != nil {
		return err
	}

	var data []byte
	err = b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == inodeNum && item.key.typ == BTRFS_INODE_ITEM_KEY {
			data = make([]byte, len(d))
			copy(data, d)
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return err
	}
	if len(data) < 100 {
		return fmt.Errorf("btrfs: inode %d not found or invalid", inodeNum)
	}

	binary.LittleEndian.PutUint64(data[16:24], uint64(size))

	now := uint64(time.Now().Unix())
	binary.LittleEndian.PutUint64(data[120:128], now)
	binary.LittleEndian.PutUint32(data[128:132], 0)
	binary.LittleEndian.PutUint64(data[132:140], now)
	binary.LittleEndian.PutUint32(data[140:144], 0)

	if err := b.updateInLeaf(b.fsRoot, key{
		objectid: inodeNum,
		typ:      BTRFS_INODE_ITEM_KEY,
		offset:   0,
	}, data); err != nil {
		return err
	}

	if size == 0 {
		var extentKeys []key
		b.walkFSTree(func(item leafItem, d []byte) error {
			if item.key.objectid == inodeNum && item.key.typ == BTRFS_EXTENT_DATA_KEY {
				extentKeys = append(extentKeys, item.key)
			}
			return nil
		})
		for _, ek := range extentKeys {
			if err := b.deleteFromLeaf(b.fsRoot, ek); err != nil {
				return err
			}
		}
	} else {

		var extKey key
		var extData []byte
		b.walkFSTree(func(item leafItem, d []byte) error {
			if item.key.objectid == inodeNum && item.key.typ == BTRFS_EXTENT_DATA_KEY {
				extKey = item.key
				extData = make([]byte, len(d))
				copy(extData, d)
			}
			return nil
		})
		if len(extData) > 0 {
			extType := extData[20]
			if extType == BTRFS_FILE_EXTENT_INLINE {
				if size <= 2048 {

					header := make([]byte, 21)
					copy(header, extData[:21])

					oldInlineData := extData[21:]
					newInlineData := make([]byte, size)
					copy(newInlineData, oldInlineData)

					binary.LittleEndian.PutUint64(header[8:16], uint64(size))
					newExtData := append(header, newInlineData...)

					if err := b.updateInLeaf(b.fsRoot, extKey, newExtData); err != nil {
						return err
					}
				} else {

					oldInlineData := extData[21:]
					newContent := make([]byte, size)
					copy(newContent, oldInlineData)

					logicalAddr, physAddr, err := b.allocateSpace(uint64(size), BTRFS_BLOCK_GROUP_DATA)
					if err != nil {
						return err
					}

					sectorSize := uint64(b.sb.SectorSize)
					if sectorSize == 0 {
						sectorSize = 4096
					}
					alignedSize := ((uint64(size) + sectorSize - 1) / sectorSize) * sectorSize

					diskBuf := make([]byte, alignedSize)
					copy(diskBuf, newContent)

					if _, err := b.dev.WriteAt(diskBuf, int64(physAddr)); err != nil {
						return err
					}

					_ = b.registerExtent(logicalAddr, alignedSize)
					_ = b.deleteFromLeaf(b.fsRoot, extKey)

					newExtItem := b.makeExtentDataRegular(logicalAddr, alignedSize, uint64(size))
					if err := b.insertIntoLeaf(b.fsRoot, key{
						objectid: inodeNum,
						typ:      BTRFS_EXTENT_DATA_KEY,
						offset:   0,
					}, newExtItem); err != nil {
						return err
					}
				}
			} else if extType == BTRFS_FILE_EXTENT_REG {

				diskBytenr := binary.LittleEndian.Uint64(extData[21:29])
				numBytes := binary.LittleEndian.Uint64(extData[45:53])

				var oldContent []byte
				if diskBytenr > 0 && numBytes > 0 {
					phys, err := b.tc.resolve(diskBytenr)
					if err == nil {
						oldContent = make([]byte, numBytes)
						_, _ = b.dev.ReadAt(oldContent, int64(phys))
					}
				}

				newContent := make([]byte, size)
				if len(oldContent) > 0 {
					copy(newContent, oldContent)
				}

				logicalAddr, physAddr, err := b.allocateSpace(uint64(size), BTRFS_BLOCK_GROUP_DATA)
				if err != nil {
					return err
				}

				sectorSize := uint64(b.sb.SectorSize)
				if sectorSize == 0 {
					sectorSize = 4096
				}
				alignedSize := ((uint64(size) + sectorSize - 1) / sectorSize) * sectorSize

				diskBuf := make([]byte, alignedSize)
				copy(diskBuf, newContent)

				if _, err := b.dev.WriteAt(diskBuf, int64(physAddr)); err != nil {
					return err
				}

				_ = b.registerExtent(logicalAddr, alignedSize)
				_ = b.deleteFromLeaf(b.fsRoot, extKey)

				newExtItem := b.makeExtentDataRegular(logicalAddr, alignedSize, uint64(size))
				if err := b.insertIntoLeaf(b.fsRoot, key{
					objectid: inodeNum,
					typ:      BTRFS_EXTENT_DATA_KEY,
					offset:   0,
				}, newExtItem); err != nil {
					return err
				}
			}
		}
	}

	b.pathCache.Delete(path)
	return nil
}

func (b *FileSystem) Symlink(target, newpath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	dir, name, err := splitPath(newpath)
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
		return nil
	})
	newInode := maxInode + 1

	if err := b.insertInodeItem(newInode, 0xA000|0777); err != nil {
		return err
	}

	var inodeData []byte
	err = b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == newInode && item.key.typ == BTRFS_INODE_ITEM_KEY {
			inodeData = make([]byte, len(d))
			copy(inodeData, d)
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err == nil || err.Error() != "stop" || len(inodeData) < 100 {
		return fmt.Errorf("btrfs: symlink inode creation failed")
	}
	binary.LittleEndian.PutUint64(inodeData[16:24], uint64(len(target)))
	if err := b.updateInLeaf(b.fsRoot, key{objectid: newInode, typ: BTRFS_INODE_ITEM_KEY, offset: 0}, inodeData); err != nil {
		return err
	}

	extData := makeExtentDataInline(0, []byte(target))
	if err := b.insertIntoLeaf(b.fsRoot, key{
		objectid: newInode,
		typ:      BTRFS_EXTENT_DATA_KEY,
		offset:   0,
	}, extData); err != nil {
		return err
	}

	if _, err := b.lookupDirEntry(dirInode, name); err == nil {
		return fmt.Errorf("btrfs: entry already exists: %s", name)
	}

	nextOff := b.nextDirIndex(dirInode)
	dirData := makeDirItem(newInode, name, BTRFS_FT_SYMLINK)
	if err := b.insertIntoLeaf(b.fsRoot, key{
		objectid: dirInode,
		typ:      BTRFS_DIR_INDEX_KEY,
		offset:   nextOff,
	}, dirData); err != nil {
		return err
	}

	b.pathCache.Delete(dir)
	return nil
}

func (b *FileSystem) Link(oldpath, newpath string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	oldInodeNum, err := b.resolvePath(oldpath)
	if err != nil {
		return err
	}

	oldInfo, err := b.readInodeInfo(oldInodeNum)
	if err != nil {
		return err
	}
	if oldInfo.IsDir {
		return fmt.Errorf("btrfs: cannot link a directory")
	}

	fileType := uint8(BTRFS_FT_REG_FILE)
	if oldInfo.IsSymlink {
		fileType = BTRFS_FT_SYMLINK
	}

	dir, name, err := splitPath(newpath)
	if err != nil {
		return err
	}

	dirInode, err := b.resolvePath(dir)
	if err != nil {
		return err
	}

	if _, err := b.lookupDirEntry(dirInode, name); err == nil {
		return fmt.Errorf("btrfs: entry already exists: %s", name)
	}

	var inodeData []byte
	err = b.walkFSTree(func(item leafItem, d []byte) error {
		if item.key.objectid == oldInodeNum && item.key.typ == BTRFS_INODE_ITEM_KEY {
			inodeData = make([]byte, len(d))
			copy(inodeData, d)
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err == nil || err.Error() != "stop" || len(inodeData) < 100 {
		return fmt.Errorf("btrfs: old inode not found")
	}
	nlink := binary.LittleEndian.Uint32(inodeData[40:44])
	nlink++
	binary.LittleEndian.PutUint32(inodeData[40:44], nlink)
	now := uint64(time.Now().Unix())
	binary.LittleEndian.PutUint64(inodeData[120:128], now)
	binary.LittleEndian.PutUint32(inodeData[128:132], 0)

	if err := b.updateInLeaf(b.fsRoot, key{objectid: oldInodeNum, typ: BTRFS_INODE_ITEM_KEY, offset: 0}, inodeData); err != nil {
		return err
	}

	nextOff := b.nextDirIndex(dirInode)
	dirData := makeDirItem(oldInodeNum, name, fileType)
	if err := b.insertIntoLeaf(b.fsRoot, key{
		objectid: dirInode,
		typ:      BTRFS_DIR_INDEX_KEY,
		offset:   nextOff,
	}, dirData); err != nil {
		return err
	}

	b.pathCache.Delete(dir)
	b.pathCache.Delete(oldpath)
	return nil
}

func (b *FileSystem) lookupDirEntryKey(dirInode uint64, name string) (key, uint64, error) {
	var foundKey key
	var childInode uint64
	found := false
	err := b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.objectid != dirInode {
			return nil
		}
		if item.key.typ != BTRFS_DIR_INDEX_KEY && item.key.typ != BTRFS_DIR_ITEM_KEY {
			return nil
		}
		if len(data) < 30 {
			return nil
		}
		r := &byteReader{buf: data}
		locObj := r.u64()
		r.skip(1)
		r.skip(8)
		r.skip(8)
		r.skip(2)
		nameLen := r.u16()
		r.skip(1)
		if nameLen == uint16(len(name)) && r.off+int(nameLen) <= len(data) {
			entryName := string(data[r.off : r.off+int(nameLen)])
			if entryName == name {
				childInode = locObj
				foundKey = item.key
				found = true
				return fmt.Errorf("found")
			}
		}
		return nil
	})
	if found {
		return foundKey, childInode, nil
	}
	if err != nil && err.Error() != "found" {
		return key{}, 0, err
	}
	return key{}, 0, fs.ErrNotExist
}

func (b *FileSystem) Superblock() (any, error) {
	return b.sb, nil
}

func (b *FileSystem) Getxattr(path string, name string) (int, []byte) {
	return -fuse.ENOSYS, nil
}

func (b *FileSystem) Setxattr(path string, name string, value []byte, flags int) int {
	return -fuse.ENOSYS
}

func (b *FileSystem) Listxattr(path string, fill func(name string) bool) int {
	return -fuse.ENOSYS
}

func (b *FileSystem) Removexattr(path string, name string) int {
	return -fuse.ENOSYS
}

func (b *FileSystem) findFSTreeRoot() (rootAddr uint64, rootLvl uint8, rootDirID uint64, err error) {
	err = b.tc.walkTree(b.sb.Root, b.sb.RootLevel, func(item leafItem, data []byte) error {
		if item.key.typ == BTRFS_ROOT_ITEM_KEY && item.key.objectid == 5 {
			if len(data) < 240 {
				return nil
			}
			r := &byteReader{buf: data}
			r.skip(160)
			r.skip(8)
			rootDirID = r.u64()
			rootAddr = r.u64()
			r.skip(8)
			r.skip(8)
			r.skip(8)
			r.skip(8)
			r.skip(4)
			r.skip(17)
			_ = r.u8()
			rootLvl = r.u8()
			return fmt.Errorf("stop")
		}
		return nil
	})
	if rootAddr == 0 {
		err = fmt.Errorf("btrfs: default FS tree not found")
	}
	if err != nil && err.Error() == "stop" {
		err = nil
	}
	return
}

func (b *FileSystem) resolvePath(path string) (uint64, error) {
	if path == "" || path == "/" {
		return b.rootInode, nil
	}
	if cached, ok := b.pathCache.Load(path); ok {
		return cached.(uint64), nil
	}
	curInode := b.rootInode
	clean := path
	if len(clean) > 0 && clean[0] == '/' {
		clean = clean[1:]
	}
	start := 0
	for start < len(clean) {
		end := start
		for end < len(clean) && clean[end] != '/' {
			end++
		}
		name := clean[start:end]
		if name == "" {
			start = end + 1
			continue
		}
		found, err := b.lookupDirEntry(curInode, name)
		if err != nil {
			return 0, err
		}
		curInode = found
		start = end + 1
	}
	b.pathCache.Store(path, curInode)
	return curInode, nil
}

func (b *FileSystem) lookupDirEntry(dirInode uint64, name string) (uint64, error) {
	var childInode uint64
	found := false
	err := b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.objectid != dirInode {
			return nil
		}
		if item.key.typ != BTRFS_DIR_INDEX_KEY && item.key.typ != BTRFS_DIR_ITEM_KEY {
			return nil
		}
		if len(data) < 30 {
			return nil
		}
		r := &byteReader{buf: data}
		locObj := r.u64()
		r.skip(1)
		r.skip(8)
		r.skip(8)
		r.skip(2)
		nameLen := r.u16()
		r.skip(1)
		if nameLen == uint16(len(name)) && r.off+int(nameLen) <= len(data) {
			entryName := string(data[r.off : r.off+int(nameLen)])
			if entryName == name {
				childInode = locObj
				found = true
				return fmt.Errorf("found")
			}
		}
		return nil
	})
	if found {
		return childInode, nil
	}
	if err != nil && err.Error() != "found" {
		return 0, err
	}
	return 0, fs.ErrNotExist
}

func (b *FileSystem) readInodeInfo(inodeNum uint64) (fs.NodeInfo, error) {
	info := fs.NodeInfo{Inode: inodeNum}
	err := b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.objectid != inodeNum || item.key.typ != BTRFS_INODE_ITEM_KEY {
			return nil
		}
		if len(data) < 100 {
			return nil
		}
		r := &byteReader{buf: data}
		r.skip(8)
		r.skip(8)
		info.Size = r.u64()
		_ = r.u64()
		_ = r.u64()
		info.Nlink = uint64(r.u32())
		info.Uid = r.u32()
		info.Gid = r.u32()
		info.Mode = r.u32()
		r.skip(4)
		if info.Mode&0x4000 != 0 {
			info.IsDir = true
		} else if info.Mode&0x8000 != 0 {
			info.IsRegular = true
		} else if info.Mode&0xA000 == 0xA000 {
			info.IsSymlink = true
		}
		info.Blksize = 4096
		info.Blocks = (info.Size + 4095) / 4096
		info.Atime = b.decodeTime(data, 108)
		info.Ctime = b.decodeTime(data, 120)
		info.Mtime = b.decodeTime(data, 132)
		return fmt.Errorf("stop")
	})
	if err != nil && err.Error() != "stop" {
		return fs.NodeInfo{}, err
	}
	return info, nil
}

func (b *FileSystem) decodeTime(data []byte, dataOff int) time.Time {
	if dataOff+12 > len(data) {
		return time.Unix(0, 0)
	}
	sec := int64(binary.LittleEndian.Uint64(data[dataOff:]))
	nsec := int64(binary.LittleEndian.Uint32(data[dataOff+8:]))
	return time.Unix(sec, nsec)
}

func (b *FileSystem) readDirEntries(dirInode uint64) ([]fs.DirEntry, error) {
	var entries []fs.DirEntry
	seen := make(map[string]bool)
	err := b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.objectid != dirInode {
			return nil
		}
		if item.key.typ != BTRFS_DIR_INDEX_KEY {
			return nil
		}
		if len(data) < 30 {
			return nil
		}
		r := &byteReader{buf: data}
		childInode := r.u64()
		r.skip(1)
		r.skip(8)
		r.skip(8)
		r.skip(2)
		nameLen := r.u16()
		fileType := r.u8()

		if nameLen > 0 && r.off+int(nameLen) <= len(data) {
			name := string(data[r.off : r.off+int(nameLen)])
			if seen[name] {
				return nil
			}
			seen[name] = true
			ft := uint8(fs.DirFileTypeUnknown)
			switch fileType {
			case BTRFS_FT_REG_FILE:
				ft = fs.DirFileTypeRegular
			case BTRFS_FT_DIR:
				ft = fs.DirFileTypeDir
			case BTRFS_FT_SYMLINK:
				ft = fs.DirFileTypeSymlink
			}
			entries = append(entries, fs.DirEntry{
				Inode:    childInode,
				Name:     name,
				FileType: ft,
			})
		}
		return nil
	})
	if err != nil && err != io.EOF {
		return nil, err
	}
	if entries == nil {
		entries = []fs.DirEntry{}
	}
	return entries, nil
}

func (b *FileSystem) readSymlink(inodeNum uint64) (string, error) {
	var target string
	err := b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.objectid != inodeNum || item.key.typ != BTRFS_EXTENT_DATA_KEY {
			return nil
		}
		if len(data) < 22 {
			return nil
		}
		r := &byteReader{buf: data}
		r.skip(8)
		_ = r.u64()
		ctype := r.u8()
		_ = r.u8()
		_ = r.u16()
		extType := r.u8()

		if extType == BTRFS_FILE_EXTENT_INLINE {
			raw := data[r.off:]
			if ctype != BTRFS_COMPRESS_NONE {
				dec, err := decompressData(ctype, raw, uint64(len(raw))*2)
				if err != nil {
					return err
				}
				target = string(dec)
			} else {
				target = string(raw)
			}
			return fmt.Errorf("stop")
		}
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return "", err
	}
	return target, nil
}

func (b *FileSystem) readFile(inodeNum uint64, buf []byte, ofst int64) (int, error) {
	var extents []extentInfo
	var fileSize uint64

	err := b.walkFSTree(func(item leafItem, data []byte) error {
		if item.key.objectid != inodeNum || item.key.typ != BTRFS_EXTENT_DATA_KEY {
			return nil
		}
		fileOffset := item.key.offset
		if len(data) < 22 {
			return nil
		}
		r := &byteReader{buf: data}
		r.skip(8)
		ramBytes := r.u64()
		ctype := r.u8()
		enc := r.u8()
		_ = r.u16()
		extType := r.u8()

		if extType == BTRFS_FILE_EXTENT_INLINE {
			rawData := data[r.off:]
			uncompressed := ramBytes
			if ctype == BTRFS_COMPRESS_ZSTD && enc == 1 {
			}
			extents = append(extents, extentInfo{
				fileOffset: fileOffset,
				size:       uncompressed,
				inline:     true,
				inlineData: rawData,
				compress:   ctype,
			})
			if fileOffset+uncompressed > fileSize {
				fileSize = fileOffset + uncompressed
			}
		} else if extType == BTRFS_FILE_EXTENT_REG {
			diskBytenr := r.u64()
			_ = r.u64()
			extOff := r.u64()
			numBytes := r.u64()
			uncompressed := numBytes
			if ctype != BTRFS_COMPRESS_NONE {
				uncompressed = ramBytes
			}
			extents = append(extents, extentInfo{
				fileOffset:   fileOffset,
				size:         uncompressed,
				diskBytenr:   diskBytenr + extOff,
				diskNumBytes: numBytes,
				compress:     ctype,
			})
			if fileOffset+uncompressed > fileSize {
				fileSize = fileOffset + uncompressed
			}
		}
		return nil
	})

	if err != nil && err != io.EOF {
		return 0, err
	}

	if ofst < 0 || fileSize == 0 || uint64(ofst) >= fileSize {
		return 0, nil
	}

	n, err := b.readFromExtents(extents, buf, uint64(ofst), fileSize)
	if err != nil {
		return 0, err
	}
	return n, nil
}

type extentInfo struct {
	fileOffset   uint64
	size         uint64
	inline       bool
	inlineData   []byte
	diskBytenr   uint64
	diskNumBytes uint64
	compress     uint8
}

func (b *FileSystem) readFromExtents(extents []extentInfo, buf []byte, ofst, fileSize uint64) (int, error) {
	totalRead := 0
	for _, ext := range extents {
		extStart := ext.fileOffset
		extEnd := extStart + ext.size
		if extEnd <= ofst {
			continue
		}
		if extStart >= ofst+uint64(len(buf)) {
			break
		}
		readStart := ofst
		if readStart < extStart {
			readStart = extStart
		}
		readEnd := ofst + uint64(len(buf))
		if readEnd > extEnd {
			readEnd = extEnd
		}
		if readEnd <= readStart {
			continue
		}
		dstStart := readStart - ofst
		toRead := int(readEnd - readStart)
		srcOff := readStart - extStart

		if ext.inline {
			var rawData []byte
			if ext.compress != BTRFS_COMPRESS_NONE {
				dec, err := decompressData(ext.compress, ext.inlineData, ext.size)
				if err != nil {
					return totalRead, err
				}
				rawData = dec
			} else {
				rawData = ext.inlineData
			}
			inlineEnd := uint64(len(rawData))
			if srcOff+uint64(toRead) > inlineEnd {
				toRead = int(inlineEnd - srcOff)
			}
			if toRead <= 0 {
				continue
			}
			copy(buf[dstStart:dstStart+uint64(toRead)], rawData[srcOff:])
			totalRead += toRead
		} else {
			if srcOff+uint64(toRead) > uint64(len(buf))-dstStart {
				toRead = int(uint64(len(buf)) - dstStart)
			}
			if toRead <= 0 {
				continue
			}
			if ext.compress != BTRFS_COMPRESS_NONE {
				blockBuf := make([]byte, ext.diskNumBytes)
				_, err := b.dev.ReadAt(blockBuf, int64(ext.diskBytenr))
				if err != nil {
					return totalRead, err
				}
				dec, err := decompressData(ext.compress, blockBuf, ext.size)
				if err != nil {
					return totalRead, err
				}
				if srcOff < uint64(len(dec)) {
					toCopy := uint64(toRead)
					if srcOff+toCopy > uint64(len(dec)) {
						toCopy = uint64(len(dec)) - srcOff
					}
					copy(buf[dstStart:dstStart+toCopy], dec[srcOff:srcOff+toCopy])
					totalRead += int(toCopy)
				}
			} else {
				if srcOff+uint64(toRead) > uint64(len(buf))-dstStart {
					toRead = int(uint64(len(buf)) - dstStart)
				}
				if toRead <= 0 {
					continue
				}
				phys := ext.diskBytenr + srcOff
				_, err := b.dev.ReadAt(buf[dstStart:dstStart+uint64(toRead)], int64(phys))
				if err != nil {
					return totalRead, err
				}
				totalRead += toRead
			}
		}
	}
	return totalRead, nil
}

func (b *FileSystem) walkFSTree(cb func(leafItem, []byte) error) error {
	return b.tc.walkTree(b.fsRoot, b.fsRootLvl, cb)
}

func (b *FileSystem) findExtentTreeRoot() (rootAddr uint64, rootLvl uint8, err error) {
	err = b.tc.walkTree(b.sb.Root, b.sb.RootLevel, func(item leafItem, data []byte) error {
		if item.key.typ == BTRFS_ROOT_ITEM_KEY && item.key.objectid == 2 {
			if len(data) < 240 {
				return nil
			}
			r := &byteReader{buf: data}
			r.skip(160)
			r.skip(8)
			_ = r.u64()
			rootAddr = r.u64()
			r.skip(8)
			r.skip(8)
			r.skip(8)
			r.skip(8)
			r.skip(4)
			r.skip(17)
			_ = r.u8()
			rootLvl = r.u8()
			return fmt.Errorf("stop")
		}
		return nil
	})
	if rootAddr == 0 {
		err = fmt.Errorf("btrfs: extent tree root not found")
	}
	if err != nil && err.Error() == "stop" {
		err = nil
	}
	return
}

func (b *FileSystem) walkExtentTree(cb func(leafItem, []byte) error) error {
	if b.extentRoot == 0 {
		return fmt.Errorf("btrfs: extent tree not loaded")
	}
	return b.tc.walkTree(b.extentRoot, b.extentRootLvl, cb)
}
