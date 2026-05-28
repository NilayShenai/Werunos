package host

import (
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/NilayShenai/Werunos/vfs"
	"github.com/winfsp/cgofuse/fuse"
)

type openFile struct {
	inodeNum uint32
	extents  []vfs.Extent
	size     uint64
	empty    bool
}

var _ fuse.FileSystemInterface = (*OrionFS)(nil)

type OrionFS struct {
	fuse.FileSystemBase
	ext4        *vfs.FileSystem
	fileHandles HandleTable[openFile]
	dirHandles  HandleTable[[]vfs.DirEntry2]
}

func NewOrionFS(fs *vfs.FileSystem) *OrionFS {
	return &OrionFS{ext4: fs}
}

func (j *OrionFS) Init() {
	log.Printf("[FUSE] Init() - filesystem is live, WinFsp handshake complete")
}

func (j *OrionFS) Destroy() {
	j.ext4.WriteSuperBlockPublic()
	log.Printf("[FUSE] Destroy() - filesystem unmounted")
}

func (j *OrionFS) Statfs(path string, stat *fuse.Statfs_t) int {
	sb, err := j.ext4.Superblock()
	if err != nil {
		log.Printf("[FUSE] Statfs(%q) ERROR - superblock not ready: %v", path, err)
		return -fuse.EIO
	}

	stat.Bsize = uint64(sb.BlockSize())
	stat.Frsize = stat.Bsize
	stat.Blocks = uint64(sb.S_blocks_count_lo)
	stat.Bfree = uint64(sb.S_free_blocks_count_lo)
	stat.Bavail = stat.Bfree
	stat.Files = uint64(sb.S_inodes_count)
	stat.Ffree = uint64(sb.S_free_inodes_count)
	stat.Favail = stat.Ffree
	stat.Namemax = 255

	log.Printf("[FUSE] Statfs(%q) OK - bsize=%d blocks=%d bfree=%d",
		path, stat.Bsize, stat.Blocks, stat.Bfree)
	return 0
}

func (j *OrionFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	inode, err := j.ext4.Walk(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			log.Printf("[FUSE] Getattr(%q) → ENOENT", path)
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Getattr(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}

	inodeToStat(inode, stat)
	log.Printf("[FUSE] Getattr(%q) OK - mode=0%o size=%d", path, stat.Mode, stat.Size)
	return 0
}

func (j *OrionFS) Opendir(path string) (int, uint64) {
	inode, err := j.ext4.Walk(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			log.Printf("[FUSE] Opendir(%q) → ENOENT", path)
			return -fuse.ENOENT, 0
		}
		log.Printf("[FUSE] Opendir(%q) ERROR walk: %v", path, err)
		return -fuse.EIO, 0
	}

	if !inode.IsDir() {
		log.Printf("[FUSE] Opendir(%q) → ENOTDIR (mode=0%o)", path, inode.I_mode)
		return -fuse.ENOTDIR, 0
	}

	entries, err := j.ext4.ReadDirCached(inode)
	if err != nil {
		log.Printf("[FUSE] Opendir(%q) ERROR ReadDir: %v", path, err)
		return -fuse.EIO, 0
	}

	fh := j.dirHandles.Store(entries)
	log.Printf("[FUSE] Opendir(%q) OK - fh=%d entries=%d", path, fh, len(entries))
	return 0, fh
}

func (j *OrionFS) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {

	entries, ok := j.dirHandles.Load(fh)
	if !ok {
		log.Printf("[FUSE] Readdir(%q) fh=%d → EBADF (handle not found)", path, fh)
		return -fuse.EBADF
	}

	log.Printf("[FUSE] Readdir(%q) fh=%d - filling %d entries", path, fh, len(entries))
	for i := range entries {
		e := &entries[i]

		var statPtr *fuse.Stat_t
		if inode, err := j.ext4.ReadInode(e.Inode); err == nil {
			var s fuse.Stat_t
			inodeToStat(inode, &s)
			statPtr = &s
		} else {
			log.Printf("[FUSE] Readdir(%q) - ReadInode(%d) failed for %q: %v",
				path, e.Inode, e.Name, err)
		}

		if !fill(e.Name, statPtr, 0) {
			log.Printf("[FUSE] Readdir(%q) - fill() returned false at entry %q (buffer full)", path, e.Name)
			break
		}
	}
	return 0
}

func (j *OrionFS) Releasedir(path string, fh uint64) int {
	log.Printf("[FUSE] Releasedir(%q) fh=%d", path, fh)
	j.dirHandles.Delete(fh)
	return 0
}

func (j *OrionFS) Open(path string, flags int) (int, uint64) {
	inodeNum, inode, err := j.ext4.WalkInode(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			log.Printf("[FUSE] Open(%q) → ENOENT", path)
			return -fuse.ENOENT, 0
		}
		log.Printf("[FUSE] Open(%q) ERROR walk: %v", path, err)
		return -fuse.EIO, 0
	}

	if inode.IsDir() {
		log.Printf("[FUSE] Open(%q) → EISDIR", path)
		return -fuse.EISDIR, 0
	}

	if !inode.IsRegular() {

		fh := j.fileHandles.Store(openFile{inodeNum: inodeNum, empty: true})
		log.Printf("[FUSE] Open(%q) OK (non-regular, mode=0%o) fh=%d", path, inode.I_mode, fh)
		return 0, fh
	}

	extents, err := j.ext4.ReadExtents(inode)
	if err != nil {
		log.Printf("[FUSE] Open(%q) ERROR ReadExtents: %v", path, err)
		return -fuse.EIO, 0
	}

	fh := j.fileHandles.Store(openFile{
		inodeNum: inodeNum,
		extents:  extents,
		size:     inode.Size(),
	})
	log.Printf("[FUSE] Open(%q) OK - fh=%d size=%d bytes", path, fh, inode.Size())
	return 0, fh
}

func (j *OrionFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	f, ok := j.fileHandles.Load(fh)
	if !ok {
		log.Printf("[FUSE] Read(%q) fh=%d → EBADF", path, fh)
		return -fuse.EBADF
	}

	if f.empty || ofst < 0 || uint64(ofst) >= f.size {
		return 0
	}

	n, err := j.ext4.ReadFileAt(f.extents, f.size, buff, ofst)
	if err != nil {
		log.Printf("[FUSE] Read(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}

	return n
}

func (j *OrionFS) Release(path string, fh uint64) int {
	log.Printf("[FUSE] Release(%q) fh=%d", path, fh)
	j.fileHandles.Delete(fh)
	return 0
}

func (j *OrionFS) Readlink(path string) (int, string) {
	inode, err := j.ext4.Walk(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			log.Printf("[FUSE] Readlink(%q) → ENOENT", path)
			return -fuse.ENOENT, ""
		}
		log.Printf("[FUSE] Readlink(%q) ERROR: %v", path, err)
		return -fuse.EIO, ""
	}

	if !inode.IsSymlink() {
		log.Printf("[FUSE] Readlink(%q) → EINVAL (not a symlink, mode=0%o)", path, inode.I_mode)
		return -fuse.EINVAL, ""
	}

	target, err := j.ext4.ReadSymlink(inode)
	if err != nil {
		log.Printf("[FUSE] Readlink(%q) ERROR ReadSymlink: %v", path, err)
		return -fuse.EIO, ""
	}

	log.Printf("[FUSE] Readlink(%q) → %q", path, target)
	return 0, target
}

func (j *OrionFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	f, ok := j.fileHandles.Load(fh)
	if !ok {
		log.Printf("[FUSE] Write(%q) fh=%d → EBADF", path, fh)
		return -fuse.EBADF
	}
	if f.empty {
		log.Printf("[FUSE] Write(%q) → EROFS (non-regular file)", path)
		return -fuse.EROFS
	}

	inode, err := j.ext4.ReadInode(f.inodeNum)
	if err != nil {
		log.Printf("[FUSE] Write(%q) ERROR reading inode: %v", path, err)
		return -fuse.EIO
	}

	n, err := j.ext4.WriteFileAt(inode, buff, ofst)
	if err != nil {
		log.Printf("[FUSE] Write(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}

	if err := j.ext4.WriteInode(f.inodeNum, inode); err != nil {
		log.Printf("[FUSE] Write(%q) ERROR persisting inode: %v", path, err)
		return -fuse.EIO
	}

	newSize := inode.Size()
	if inode.UsesExtents() {
		newExtents, err := j.ext4.ReadExtents(inode)
		if err == nil {
			f.extents = newExtents
		}
	}
	f.size = newSize
	j.fileHandles.Replace(fh, f)

	log.Printf("[FUSE] Write(%q) OK - %d bytes at offset %d", path, n, ofst)
	return n
}

func (j *OrionFS) Truncate(path string, size int64, fh uint64) int {
	var inodeNum uint32
	var err error
	if fh != 0 {
		f, ok := j.fileHandles.Load(fh)
		if !ok {
			log.Printf("[FUSE] Truncate(%q) fh=%d → EBADF", path, fh)
			return -fuse.EBADF
		}
		inodeNum = f.inodeNum
	} else {
		inodeNum, _, err = j.ext4.WalkInode(path)
		if err != nil {
			if isENOENT(err) {
				return -fuse.ENOENT
			}
			log.Printf("[FUSE] Truncate(%q) ERROR walk: %v", path, err)
			return -fuse.EIO
		}
	}

	inode, err := j.ext4.ReadInode(inodeNum)
	if err != nil {
		log.Printf("[FUSE] Truncate(%q) ERROR reading inode: %v", path, err)
		return -fuse.EIO
	}

	if !inode.IsRegular() {
		log.Printf("[FUSE] Truncate(%q) → EROFS (not a regular file)", path)
		return -fuse.EROFS
	}

	curSize := int64(inode.Size())
	if size == curSize {
		return 0
	}

	if size > curSize {
		zeroBuf := make([]byte, 4096)
		remaining := size - curSize
		written := int64(0)
		for written < remaining {
			toWrite := int64(len(zeroBuf))
			if toWrite > remaining-written {
				toWrite = remaining - written
			}
			n, err := j.ext4.WriteFileAt(inode, zeroBuf[:toWrite], curSize+written)
			if err != nil {
				log.Printf("[FUSE] Truncate(%q) ERROR extending: %v", path, err)
				return -fuse.EIO
			}
			written += int64(n)
		}
	} else {
		if err := j.ext4.TruncateInode(inode, uint64(size)); err != nil {
			log.Printf("[FUSE] Truncate(%q) ERROR shrinking: %v", path, err)
			return -fuse.EIO
		}
	}

	if err := j.ext4.WriteInode(inodeNum, inode); err != nil {
		log.Printf("[FUSE] Truncate(%q) ERROR persisting inode: %v", path, err)
		return -fuse.EIO
	}

	if fh != 0 {
		f, ok := j.fileHandles.Load(fh)
		if ok {
			if inode.UsesExtents() {
				newExtents, err := j.ext4.ReadExtents(inode)
				if err == nil {
					f.extents = newExtents
				}
			}
			f.size = inode.Size()
			j.fileHandles.Replace(fh, f)
		}
	}

	log.Printf("[FUSE] Truncate(%q) OK → %d bytes", path, size)
	return 0
}

func (j *OrionFS) Create(path string, flags int, mode uint32) (int, uint64) {
	parent, name, err := j.splitPath(path)
	if err != nil {
		log.Printf("[FUSE] Create(%q) ERROR splitting path: %v", path, err)
		return -fuse.ENOENT, 0
	}

	parentInode, err := j.ext4.Walk(parent)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT, 0
		}
		log.Printf("[FUSE] Create(%q) ERROR walking parent: %v", path, err)
		return -fuse.EIO, 0
	}
	parentNum, _, err := j.ext4.WalkInode(parent)
	if err != nil {
		return -fuse.EIO, 0
	}

	inodeNum, err := j.ext4.AllocateInode()
	if err != nil {
		log.Printf("[FUSE] Create(%q) ERROR allocating inode: %v", path, err)
		return -fuse.ENOSPC, 0
	}

	inode, err := j.ext4.InitNewInode(inodeNum, uint16(vfs.S_IFREG|uint16(mode&0x0FFF)))
	if err != nil {
		log.Printf("[FUSE] Create(%q) ERROR init inode: %v", path, err)
		return -fuse.EIO, 0
	}

	if err := j.ext4.AddDirEntry(parentInode, name, inodeNum, vfs.DirFileTypeRegular); err != nil {
		log.Printf("[FUSE] Create(%q) ERROR adding dir entry: %v", path, err)
		return -fuse.EIO, 0
	}

	parentInode.I_mtime = uint32(time.Now().Unix())
	if err := j.ext4.WriteInode(parentNum, parentInode); err != nil {
		return -fuse.EIO, 0
	}

	extents, err := j.ext4.ReadExtents(inode)
	if err != nil {
		return -fuse.EIO, 0
	}
	fh := j.fileHandles.Store(openFile{
		inodeNum: inodeNum,
		extents:  extents,
		size:     inode.Size(),
	})

	log.Printf("[FUSE] Create(%q) OK - inode=%d fh=%d", path, inodeNum, fh)
	return 0, fh
}

func (j *OrionFS) Mkdir(path string, mode uint32) int {
	parent, name, err := j.splitPath(path)
	if err != nil {
		log.Printf("[FUSE] Mkdir(%q) ERROR splitting path: %v", path, err)
		return -fuse.ENOENT
	}

	parentInode, err := j.ext4.Walk(parent)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Mkdir(%q) ERROR walking parent: %v", path, err)
		return -fuse.EIO
	}
	parentNum, _, err := j.ext4.WalkInode(parent)
	if err != nil {
		return -fuse.EIO
	}

	inodeNum, err := j.ext4.AllocateInode()
	if err != nil {
		log.Printf("[FUSE] Mkdir(%q) ERROR allocating inode: %v", path, err)
		return -fuse.ENOSPC
	}

	dirMode := uint16(vfs.S_IFDIR | uint16(mode&0x0FFF))
	inode, err := j.ext4.InitNewInode(inodeNum, dirMode)
	if err != nil {
		log.Printf("[FUSE] Mkdir(%q) ERROR init inode: %v", path, err)
		return -fuse.EIO
	}

	blk, err := j.ext4.AllocateBlock()
	if err != nil {
		return -fuse.ENOSPC
	}

	buf := make([]byte, j.ext4.BlockSize)

	binary.LittleEndian.PutUint32(buf[0:4], inodeNum)
	binary.LittleEndian.PutUint16(buf[4:6], 12)
	buf[6] = 1
	buf[7] = 2
	buf[8] = '.'

	binary.LittleEndian.PutUint32(buf[12:16], parentNum)
	remaining := int(j.ext4.BlockSize) - 12
	binary.LittleEndian.PutUint16(buf[16:18], uint16(remaining))
	buf[18] = 2
	buf[19] = 2
	buf[20] = '.'
	buf[21] = '.'

	if err := j.ext4.WriteBlock(blk, buf); err != nil {
		return -fuse.EIO
	}

	if err := j.ext4.AppendExtent(inode, vfs.Extent{
		EE_block:    0,
		EE_len:      1,
		EE_start_lo: uint32(blk & 0xFFFFFFFF),
		EE_start_hi: uint16(blk >> 32),
	}); err != nil {
		return -fuse.EIO
	}
	inode.I_size_lo = uint32(j.ext4.BlockSize)
	blockSectors := j.ext4.BlockSize / 512
	inode.I_blocks_lo = uint32(blockSectors)

	if err := j.ext4.WriteInode(inodeNum, inode); err != nil {
		return -fuse.EIO
	}

	if err := j.ext4.AddDirEntry(parentInode, name, inodeNum, vfs.DirFileTypeDir); err != nil {
		log.Printf("[FUSE] Mkdir(%q) ERROR adding dir entry: %v", path, err)
		return -fuse.EIO
	}

	parentInode.I_links_count++
	parentInode.I_mtime = uint32(time.Now().Unix())
	if err := j.ext4.WriteInode(parentNum, parentInode); err != nil {
		return -fuse.EIO
	}

	log.Printf("[FUSE] Mkdir(%q) OK - inode=%d", path, inodeNum)
	return 0
}

func (j *OrionFS) splitPath(path string) (parent, name string, err error) {
	if path == "/" {
		return "", "", fmt.Errorf("cannot split root path")
	}

	idx := strings.LastIndex(path, "/")
	if idx == 0 {
		parent = "/"
	} else {
		parent = path[:idx]
	}
	name = path[idx+1:]
	if name == "" {
		return "", "", fmt.Errorf("empty leaf name")
	}
	return parent, name, nil
}

func (j *OrionFS) Unlink(path string) int {
	return j.removeEntry(path, false)
}

func (j *OrionFS) Rmdir(path string) int {
	return j.removeEntry(path, true)
}

func (j *OrionFS) removeEntry(path string, mustBeDir bool) int {
	parent, name, err := j.splitPath(path)
	if err != nil {
		return -fuse.ENOENT
	}

	parentInode, err := j.ext4.Walk(parent)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	parentNum, _, err := j.ext4.WalkInode(parent)
	if err != nil {
		return -fuse.EIO
	}

	childInodeNum, err := j.ext4.Lookup(parentInode, name)
	if err != nil {
		return -fuse.ENOENT
	}

	childInode, err := j.ext4.ReadInode(childInodeNum)
	if err != nil {
		return -fuse.EIO
	}

	if mustBeDir && !childInode.IsDir() {
		return -fuse.ENOTDIR
	}
	if !mustBeDir && childInode.IsDir() {
		return -fuse.EISDIR
	}

	if childInode.IsDir() {

		entries, err := j.ext4.ReadDir(childInode)
		if err != nil {
			return -fuse.EIO
		}
		realEntries := 0
		for _, e := range entries {
			if e.Name != "." && e.Name != ".." {
				realEntries++
			}
		}
		if realEntries > 0 {
			return -fuse.ENOTEMPTY
		}
	}

	if err := j.ext4.RemoveDirEntry(parentInode, name); err != nil {
		return -fuse.EIO
	}

	childInode.I_links_count--
	childInode.I_ctime = uint32(time.Now().Unix())

	if childInode.I_links_count == 0 {
		childInode.I_dtime = uint32(time.Now().Unix())

		if childInode.UsesExtents() {
			extents, err := j.ext4.ReadExtents(childInode)
			if err == nil {
				if err := j.ext4.FreeExtents(extents); err != nil {
					log.Printf("[FUSE] removeEntry(%q) WARNING freeing blocks: %v", path, err)
				}
			}
		}

		if err := j.ext4.FreeInode(childInodeNum); err != nil {
			log.Printf("[FUSE] removeEntry(%q) WARNING freeing inode: %v", path, err)
		}
	}

	if err := j.ext4.WriteInode(childInodeNum, childInode); err != nil {
		return -fuse.EIO
	}

	if childInode.IsDir() {
		parentInode.I_links_count--
	}
	parentInode.I_mtime = uint32(time.Now().Unix())
	if err := j.ext4.WriteInode(parentNum, parentInode); err != nil {
		return -fuse.EIO
	}

	log.Printf("[FUSE] removeEntry(%q) OK", path)
	return 0
}

func (j *OrionFS) Rename(oldpath string, newpath string) int {
	oldParent, oldName, err := j.splitPath(oldpath)
	if err != nil {
		return -fuse.ENOENT
	}
	newParent, newName, err := j.splitPath(newpath)
	if err != nil {
		return -fuse.ENOENT
	}

	oldParentInode, err := j.ext4.Walk(oldParent)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	oldParentNum, _, err := j.ext4.WalkInode(oldParent)
	if err != nil {
		return -fuse.EIO
	}

	newParentInode, err := j.ext4.Walk(newParent)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	newParentNum, _, err := j.ext4.WalkInode(newParent)
	if err != nil {
		return -fuse.EIO
	}

	childInodeNum, err := j.ext4.Lookup(oldParentInode, oldName)
	if err != nil {
		return -fuse.ENOENT
	}

	childInode, err := j.ext4.ReadInode(childInodeNum)
	if err != nil {
		return -fuse.EIO
	}

	if existingNum, err := j.ext4.Lookup(newParentInode, newName); err == nil {
		existingInode, err := j.ext4.ReadInode(existingNum)
		if err != nil {
			return -fuse.EIO
		}
		if existingInode.IsDir() {
			return -fuse.EISDIR
		}
		if err := j.ext4.RemoveDirEntry(newParentInode, newName); err != nil {
			return -fuse.EIO
		}
		existingInode.I_links_count--
		existingInode.I_ctime = uint32(time.Now().Unix())
		if existingInode.I_links_count == 0 {
			existingInode.I_dtime = uint32(time.Now().Unix())
			if existingInode.UsesExtents() {
				if extents, err := j.ext4.ReadExtents(existingInode); err == nil {
					j.ext4.FreeExtents(extents)
				}
			}
			j.ext4.FreeInode(existingNum)
		}
		if err := j.ext4.WriteInode(existingNum, existingInode); err != nil {
			return -fuse.EIO
		}
	}

	if err := j.ext4.RemoveDirEntry(oldParentInode, oldName); err != nil {
		return -fuse.EIO
	}

	fileType := uint8(vfs.DirFileTypeRegular)
	if childInode.IsDir() {
		fileType = uint8(vfs.DirFileTypeDir)
	}
	if err := j.ext4.AddDirEntry(newParentInode, newName, childInodeNum, fileType); err != nil {

		j.ext4.AddDirEntry(oldParentInode, oldName, childInodeNum, fileType)
		return -fuse.EIO
	}

	now := uint32(time.Now().Unix())
	oldParentInode.I_mtime = now
	newParentInode.I_mtime = now
	if err := j.ext4.WriteInode(oldParentNum, oldParentInode); err != nil {
		return -fuse.EIO
	}
	if err := j.ext4.WriteInode(newParentNum, newParentInode); err != nil {
		return -fuse.EIO
	}

	log.Printf("[FUSE] Rename(%q → %q) OK", oldpath, newpath)
	return 0
}

func (j *OrionFS) Chmod(path string, mode uint32) int {
	inodeNum, inode, err := j.ext4.WalkInode(path)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}

	inode.I_mode = (inode.I_mode & 0xF000) | uint16(mode&0x0FFF)
	inode.I_ctime = uint32(time.Now().Unix())

	if err := j.ext4.WriteInode(inodeNum, inode); err != nil {
		return -fuse.EIO
	}
	log.Printf("[FUSE] Chmod(%q) → 0%o", path, mode)
	return 0
}

func (j *OrionFS) Chown(path string, uid uint32, gid uint32) int {
	inodeNum, inode, err := j.ext4.WalkInode(path)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}

	inode.I_uid = uint16(uid & 0xFFFF)
	inode.I_gid = uint16(gid & 0xFFFF)
	inode.L_i_uid_high = uint16(uid >> 16)
	inode.L_i_gid_high = uint16(gid >> 16)
	inode.I_ctime = uint32(time.Now().Unix())

	if err := j.ext4.WriteInode(inodeNum, inode); err != nil {
		return -fuse.EIO
	}
	log.Printf("[FUSE] Chown(%q) → uid=%d gid=%d", path, uid, gid)
	return 0
}

func (j *OrionFS) Symlink(target string, newpath string) int {
	parent, name, err := j.splitPath(newpath)
	if err != nil {
		log.Printf("[FUSE] Symlink(%q → %q) → ENOENT (bad newpath)", target, newpath)
		return -fuse.ENOENT
	}

	parentInode, err := j.ext4.Walk(parent)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	parentNum, _, err := j.ext4.WalkInode(parent)
	if err != nil {
		return -fuse.EIO
	}

	inodeNum, err := j.ext4.AllocateInode()
	if err != nil {
		return -fuse.ENOSPC
	}

	now := uint32(time.Now().Unix())
	symMode := uint16(vfs.S_IFLNK | 0o777)
	inode := &vfs.Inode{
		I_mode:  symMode,
		I_uid:   0,
		I_gid:   0,
		I_size_lo: uint32(len(target)),
		I_atime: now,
		I_ctime: now,
		I_mtime: now,
		I_links_count: 1,
	}

	if len(target) <= 60 {
		copy(inode.I_block[:], target)
	} else {

		vfs.InitExtentHeader(inode)
		blk, err := j.ext4.AllocateBlock()
		if err != nil {
			return -fuse.ENOSPC
		}
		if err := j.ext4.AppendExtent(inode, vfs.Extent{
			EE_block:    0,
			EE_len:      1,
			EE_start_lo: uint32(blk & 0xFFFFFFFF),
			EE_start_hi: uint16(blk >> 32),
		}); err != nil {
			return -fuse.EIO
		}
		inode.I_size_lo = uint32(len(target))
		inode.I_blocks_lo = uint32(j.ext4.BlockSize / 512)

		blockData := make([]byte, j.ext4.BlockSize)
		copy(blockData, target)
		if err := j.ext4.WriteBlock(blk, blockData); err != nil {
			return -fuse.EIO
		}
	}

	if err := j.ext4.WriteInode(inodeNum, inode); err != nil {
		return -fuse.EIO
	}

	if err := j.ext4.AddDirEntry(parentInode, name, inodeNum, vfs.DirFileTypeSymlink); err != nil {
		return -fuse.EIO
	}

	parentInode.I_mtime = now
	if err := j.ext4.WriteInode(parentNum, parentInode); err != nil {
		return -fuse.EIO
	}

	log.Printf("[FUSE] Symlink(%q → %q) OK", target, newpath)
	return 0
}

func (j *OrionFS) Link(oldpath string, newpath string) int {
	targetNum, targetInode, err := j.ext4.WalkInode(oldpath)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}

	parent, name, err := j.splitPath(newpath)
	if err != nil {
		return -fuse.ENOENT
	}

	parentInode, err := j.ext4.Walk(parent)
	if err != nil {
		if isENOENT(err) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}
	parentNum, _, err := j.ext4.WalkInode(parent)
	if err != nil {
		return -fuse.EIO
	}

	if targetInode.IsDir() {
		return -fuse.EPERM
	}

	fileType := uint8(vfs.DirFileTypeRegular)
	if targetInode.IsSymlink() {
		fileType = uint8(vfs.DirFileTypeSymlink)
	}

	if err := j.ext4.AddDirEntry(parentInode, name, targetNum, fileType); err != nil {
		return -fuse.EIO
	}

	targetInode.I_links_count++
	if err := j.ext4.WriteInode(targetNum, targetInode); err != nil {
		return -fuse.EIO
	}

	parentInode.I_mtime = uint32(time.Now().Unix())
	if err := j.ext4.WriteInode(parentNum, parentInode); err != nil {
		return -fuse.EIO
	}

	log.Printf("[FUSE] Link(%q → %q) OK", oldpath, newpath)
	return 0
}

func inodeToStat(inode *vfs.Inode, stat *fuse.Stat_t) {
	mode := uint32(inode.I_mode)

	mode |= 0664

	if inode.IsDir() {
		mode |= 0111
	}
	stat.Mode = mode
	stat.Nlink = uint32(inode.I_links_count)
	stat.Size = int64(inode.Size())
	stat.Atim = fuse.NewTimespec(time.Unix(int64(inode.I_atime), 0))
	stat.Ctim = fuse.NewTimespec(time.Unix(int64(inode.I_ctime), 0))
	stat.Mtim = fuse.NewTimespec(time.Unix(int64(inode.I_mtime), 0))
	stat.Blksize = 512
	stat.Blocks = int64(inode.I_blocks_lo)
}

func isENOENT(err error) bool {
	return err != nil && (errors.Is(err, vfs.ErrNotExist) || err.Error() == "no such file or directory")
}

func fmtErrc(errc int) string {
	if errc == 0 {
		return "OK"
	}
	return fmt.Sprintf("error(%d)", errc)
}
