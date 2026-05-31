package ext4

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NilayShenai/Werunos/fs"
	"github.com/NilayShenai/Werunos/vfs"
	"github.com/winfsp/cgofuse/fuse"
)

type openFile struct {
	inodeNum uint32
	extents  []vfs.Extent
	size     uint64
	empty    bool
	refs     int32
}

type dirCache struct {
	entries []fs.DirEntry
}

type FileSystem struct {
	inner *vfs.FileSystem
	dev   vfs.ReadWriterAt

	mu    sync.Mutex
	files map[string]*openFile
	dirs  map[string]*dirCache

	nextFH atomic.Uint64
	fhMap  sync.Map
}

func New(dev vfs.ReadWriterAt) *FileSystem {
	return &FileSystem{
		dev:   dev,
		files: make(map[string]*openFile),
		dirs:  make(map[string]*dirCache),
	}
}

func (e *FileSystem) Type() string { return "ext4" }

func (e *FileSystem) ReadSuperBlock() error {
	var err error
	e.inner, err = vfs.NewFileSystem(e.dev)
	if err != nil {
		return err
	}
	if _, err := e.inner.ReadSuperBlock(); err != nil {
		return err
	}
	return e.inner.ReadGroupDescriptors()
}

func (e *FileSystem) BlockSize() uint64 { return e.inner.BlockSize }

func (e *FileSystem) Destroy() {
	e.inner.WriteSuperBlockPublic()
}

func toNodeInfo(inode *vfs.Inode) fs.NodeInfo {
	return fs.NodeInfo{
		Mode:      uint32(inode.I_mode),
		Uid:       uint32(inode.I_uid) | (uint32(inode.L_i_uid_high) << 16),
		Gid:       uint32(inode.I_gid) | (uint32(inode.L_i_gid_high) << 16),
		Size:      inode.Size(),
		Atime:     time.Unix(int64(inode.I_atime), 0),
		Mtime:     time.Unix(int64(inode.I_mtime), 0),
		Ctime:     time.Unix(int64(inode.I_ctime), 0),
		Nlink:     uint64(inode.I_links_count),
		Blksize:   512,
		Blocks:    uint64(inode.I_blocks_lo),
		IsDir:     inode.IsDir(),
		IsRegular: inode.IsRegular(),
		IsSymlink: inode.IsSymlink(),
	}
}

func (e *FileSystem) Getattr(path string) (fs.NodeInfo, error) {
	inode, err := e.inner.Walk(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			return fs.NodeInfo{}, fs.ErrNotExist
		}
		return fs.NodeInfo{}, err
	}
	return toNodeInfo(inode), nil
}

func (e *FileSystem) Readdir(path string) ([]fs.DirEntry, error) {
	e.mu.Lock()
	if cached, ok := e.dirs[path]; ok {
		e.mu.Unlock()
		return cached.entries, nil
	}
	e.mu.Unlock()

	inode, err := e.inner.Walk(path)
	if err != nil {
		return nil, err
	}
	entries, err := e.inner.ReadDir(inode)
	if err != nil {
		return nil, err
	}
	result := make([]fs.DirEntry, len(entries))
	for i, ent := range entries {
		result[i] = fs.DirEntry{
			Inode:    uint64(ent.Inode),
			Name:     ent.Name,
			FileType: ent.FileType,
		}
	}

	e.mu.Lock()
	e.dirs[path] = &dirCache{entries: result}
	e.mu.Unlock()
	return result, nil
}

func (e *FileSystem) Readlink(path string) (string, error) {
	inode, err := e.inner.Walk(path)
	if err != nil {
		return "", err
	}
	return e.inner.ReadSymlink(inode)
}

func (e *FileSystem) Open(path string, flags int) error {
	inodeNum, inode, err := e.inner.WalkInode(path)
	if err != nil {
		return err
	}
	f := &openFile{inodeNum: inodeNum, refs: 1}
	if inode.IsRegular() && inode.UsesExtents() {
		extents, err := e.inner.ReadExtents(inode)
		if err != nil {
			return err
		}
		f.extents = extents
		f.size = inode.Size()
	} else {
		f.empty = true
	}
	e.mu.Lock()
	e.files[path] = f
	e.mu.Unlock()
	return nil
}

func (e *FileSystem) Read(path string, buf []byte, ofst int64) (int, error) {
	e.mu.Lock()
	f, ok := e.files[path]
	e.mu.Unlock()
	if !ok {
		return 0, io.EOF
	}
	if f.empty || ofst < 0 || uint64(ofst) >= f.size {
		return 0, nil
	}
	return e.inner.ReadFileAt(f.extents, f.size, buf, ofst)
}

func (e *FileSystem) Release(path string) error {
	e.mu.Lock()
	delete(e.files, path)
	e.mu.Unlock()
	return nil
}

func (e *FileSystem) Create(path string, flags int, mode uint32) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	parent, name, err := splitPath(path)
	if err != nil {
		return err
	}
	parentInode, err := e.inner.Walk(parent)
	if err != nil {
		return err
	}
	parentNum, _, err := e.inner.WalkInode(parent)
	if err != nil {
		return err
	}
	inodeNum, err := e.inner.AllocateInode()
	if err != nil {
		return err
	}
	inode, err := e.inner.InitNewInode(inodeNum, uint16(vfs.S_IFREG|uint16(mode&0x0FFF)))
	if err != nil {
		return err
	}
	if err := e.inner.AddDirEntry(parentInode, name, inodeNum, vfs.DirFileTypeRegular); err != nil {
		return err
	}
	parentInode.I_mtime = uint32(time.Now().Unix())
	if err := e.inner.WriteInode(parentNum, parentInode); err != nil {
		return err
	}
	e.mu.Lock()
	extents, _ := e.inner.ReadExtents(inode)
	e.files[path] = &openFile{
		inodeNum: inodeNum,
		extents:  extents,
		size:     inode.Size(),
		refs:     1,
	}
	e.mu.Unlock()
	_ = inode
	return nil
}

func (e *FileSystem) Write(path string, buf []byte, ofst int64) (n int, err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	e.mu.Lock()
	f, ok := e.files[path]
	e.mu.Unlock()
	if !ok {
		return 0, io.EOF
	}
	if f.empty {
		return 0, fmt.Errorf("read-only file")
	}
	inode, err := e.inner.ReadInode(f.inodeNum)
	if err != nil {
		return 0, err
	}
	n, err = e.inner.WriteFileAt(inode, buf, ofst)
	if err != nil {
		return 0, err
	}
	if err := e.inner.WriteInode(f.inodeNum, inode); err != nil {
		return 0, err
	}
	newSize := inode.Size()
	if inode.UsesExtents() {
		newExtents, err := e.inner.ReadExtents(inode)
		if err == nil {
			f.extents = newExtents
		}
	}
	f.size = newSize
	e.mu.Lock()
	e.files[path] = f
	e.mu.Unlock()
	return n, nil
}

func (e *FileSystem) Mkdir(path string, mode uint32) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	parent, name, err := splitPath(path)
	if err != nil {
		return err
	}
	parentInode, err := e.inner.Walk(parent)
	if err != nil {
		return err
	}
	parentNum, _, err := e.inner.WalkInode(parent)
	if err != nil {
		return err
	}
	inodeNum, err := e.inner.AllocateInode()
	if err != nil {
		return err
	}
	dirMode := uint16(vfs.S_IFDIR | uint16(mode&0x0FFF))
	inode, err := e.inner.InitNewInode(inodeNum, dirMode)
	if err != nil {
		return err
	}

	blk, err := e.inner.AllocateBlock()
	if err != nil {
		return err
	}
	_ = inode
	_ = blk

	if err := e.inner.AddDirEntry(parentInode, name, inodeNum, vfs.DirFileTypeDir); err != nil {
		return err
	}

	parentInode.I_links_count++
	parentInode.I_mtime = uint32(time.Now().Unix())
	if err := e.inner.WriteInode(parentNum, parentInode); err != nil {
		return err
	}

	e.mu.Lock()
	delete(e.dirs, parent)
	e.mu.Unlock()
	return nil
}

func (e *FileSystem) Unlink(path string) error {
	return e.removeEntry(path, false)
}

func (e *FileSystem) Rmdir(path string) error {
	return e.removeEntry(path, true)
}

func (e *FileSystem) removeEntry(path string, mustBeDir bool) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	parent, name, err := splitPath(path)
	if err != nil {
		return err
	}
	parentInode, err := e.inner.Walk(parent)
	if err != nil {
		return err
	}
	parentNum, _, err := e.inner.WalkInode(parent)
	if err != nil {
		return err
	}
	childInodeNum, err := e.inner.Lookup(parentInode, name)
	if err != nil {
		return err
	}
	childInode, err := e.inner.ReadInode(childInodeNum)
	if err != nil {
		return err
	}
	if mustBeDir && !childInode.IsDir() {
		return fmt.Errorf("not a directory")
	}
	if !mustBeDir && childInode.IsDir() {
		return fmt.Errorf("is a directory")
	}
	if childInode.IsDir() {
		entries, err := e.inner.ReadDir(childInode)
		if err != nil {
			return err
		}
		realEntries := 0
		for _, en := range entries {
			if en.Name != "." && en.Name != ".." {
				realEntries++
			}
		}
		if realEntries > 0 {
			return fmt.Errorf("directory not empty")
		}
	}
	if err := e.inner.RemoveDirEntry(parentInode, name); err != nil {
		return err
	}
	childInode.I_links_count--
	childInode.I_ctime = uint32(time.Now().Unix())
	if childInode.I_links_count == 0 {
		childInode.I_dtime = uint32(time.Now().Unix())
		if childInode.UsesExtents() {
			extents, err := e.inner.ReadExtents(childInode)
			if err == nil {
				e.inner.FreeExtents(extents)
			}
		}
		e.inner.FreeInode(childInodeNum)
	}
	if err := e.inner.WriteInode(childInodeNum, childInode); err != nil {
		return err
	}
	if childInode.IsDir() {
		parentInode.I_links_count--
	}
	parentInode.I_mtime = uint32(time.Now().Unix())
	if err := e.inner.WriteInode(parentNum, parentInode); err != nil {
		return err
	}

	e.mu.Lock()
	delete(e.dirs, parent)
	e.mu.Unlock()
	return nil
}

func (e *FileSystem) Rename(oldpath, newpath string) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	oldParent, oldName, err := splitPath(oldpath)
	if err != nil {
		return err
	}
	newParent, newName, err := splitPath(newpath)
	if err != nil {
		return err
	}
	oldParentInode, err := e.inner.Walk(oldParent)
	if err != nil {
		return err
	}
	oldParentNum, _, err := e.inner.WalkInode(oldParent)
	if err != nil {
		return err
	}
	newParentInode, err := e.inner.Walk(newParent)
	if err != nil {
		return err
	}
	newParentNum, _, err := e.inner.WalkInode(newParent)
	if err != nil {
		return err
	}
	childInodeNum, err := e.inner.Lookup(oldParentInode, oldName)
	if err != nil {
		return err
	}
	childInode, err := e.inner.ReadInode(childInodeNum)
	if err != nil {
		return err
	}
	if existingNum, err := e.inner.Lookup(newParentInode, newName); err == nil {
		existingInode, err := e.inner.ReadInode(existingNum)
		if err != nil {
			return err
		}
		if existingInode.IsDir() {
			return fmt.Errorf("is a directory")
		}
		if err := e.inner.RemoveDirEntry(newParentInode, newName); err != nil {
			return err
		}
		existingInode.I_links_count--
		existingInode.I_ctime = uint32(time.Now().Unix())
		if existingInode.I_links_count == 0 {
			existingInode.I_dtime = uint32(time.Now().Unix())
			if existingInode.UsesExtents() {
				if extents, err := e.inner.ReadExtents(existingInode); err == nil {
					e.inner.FreeExtents(extents)
				}
			}
			e.inner.FreeInode(existingNum)
		}
		if err := e.inner.WriteInode(existingNum, existingInode); err != nil {
			return err
		}
	}
	if err := e.inner.RemoveDirEntry(oldParentInode, oldName); err != nil {
		return err
	}
	fileType := uint8(vfs.DirFileTypeRegular)
	if childInode.IsDir() {
		fileType = uint8(vfs.DirFileTypeDir)
	}
	if err := e.inner.AddDirEntry(newParentInode, newName, childInodeNum, fileType); err != nil {
		e.inner.AddDirEntry(oldParentInode, oldName, childInodeNum, fileType)
		return err
	}
	now := uint32(time.Now().Unix())
	oldParentInode.I_mtime = now
	newParentInode.I_mtime = now
	if err := e.inner.WriteInode(oldParentNum, oldParentInode); err != nil {
		return err
	}
	if err := e.inner.WriteInode(newParentNum, newParentInode); err != nil {
		return err
	}

	e.mu.Lock()
	delete(e.dirs, oldParent)
	delete(e.dirs, newParent)
	e.mu.Unlock()
	return nil
}

func (e *FileSystem) Chmod(path string, mode uint32) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	inodeNum, inode, err := e.inner.WalkInode(path)
	if err != nil {
		return err
	}
	inode.I_mode = (inode.I_mode & 0xF000) | uint16(mode&0x0FFF)
	inode.I_ctime = uint32(time.Now().Unix())
	return e.inner.WriteInode(inodeNum, inode)
}

func (e *FileSystem) Chown(path string, uid, gid uint32) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	inodeNum, inode, err := e.inner.WalkInode(path)
	if err != nil {
		return err
	}
	inode.I_uid = uint16(uid & 0xFFFF)
	inode.I_gid = uint16(gid & 0xFFFF)
	inode.L_i_uid_high = uint16(uid >> 16)
	inode.L_i_gid_high = uint16(gid >> 16)
	inode.I_ctime = uint32(time.Now().Unix())
	return e.inner.WriteInode(inodeNum, inode)
}

func (e *FileSystem) Truncate(path string, size int64) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	inodeNum, inode, err := e.inner.WalkInode(path)
	if err != nil {
		return err
	}
	if !inode.IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	curSize := int64(inode.Size())
	if size == curSize {
		return nil
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
			_, err := e.inner.WriteFileAt(inode, zeroBuf[:toWrite], curSize+written)
			if err != nil {
				return err
			}
			written += toWrite
		}
	} else {
		if err := e.inner.TruncateInode(inode, uint64(size)); err != nil {
			return err
		}
	}
	return e.inner.WriteInode(inodeNum, inode)
}

func (e *FileSystem) Symlink(target, newpath string) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	parent, name, err := splitPath(newpath)
	if err != nil {
		return err
	}
	parentInode, err := e.inner.Walk(parent)
	if err != nil {
		return err
	}
	parentNum, _, err := e.inner.WalkInode(parent)
	if err != nil {
		return err
	}
	inodeNum, err := e.inner.AllocateInode()
	if err != nil {
		return err
	}
	now := uint32(time.Now().Unix())
	symMode := uint16(vfs.S_IFLNK | 0o777)
	inode := &vfs.Inode{
		I_mode:       symMode,
		I_uid:        0,
		I_gid:        0,
		I_size_lo:    uint32(len(target)),
		I_atime:      now,
		I_ctime:      now,
		I_mtime:      now,
		I_links_count: 1,
	}
	if len(target) <= 60 {
		copy(inode.I_block[:], target)
	} else {
		vfs.InitExtentHeader(inode)
		blk, err := e.inner.AllocateBlock()
		if err != nil {
			return err
		}
		if err := e.inner.AppendExtent(inode, vfs.Extent{
			EE_block:    0,
			EE_len:      1,
			EE_start_lo: uint32(blk & 0xFFFFFFFF),
			EE_start_hi: uint16(blk >> 32),
		}); err != nil {
			return err
		}
		inode.I_size_lo = uint32(len(target))
		inode.I_blocks_lo = uint32(e.inner.BlockSize / 512)
		blockData := make([]byte, e.inner.BlockSize)
		copy(blockData, target)
		if err := e.inner.WriteBlock(blk, blockData); err != nil {
			return err
		}
	}
	if err := e.inner.WriteInode(inodeNum, inode); err != nil {
		return err
	}
	if err := e.inner.AddDirEntry(parentInode, name, inodeNum, vfs.DirFileTypeSymlink); err != nil {
		return err
	}
	parentInode.I_mtime = now
	return e.inner.WriteInode(parentNum, parentInode)
}

func (e *FileSystem) Link(oldpath, newpath string) (err error) {
	e.inner.BeginTransaction()
	defer func() {
		if err != nil {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				err = commitErr
			}
		}
	}()

	targetNum, targetInode, err := e.inner.WalkInode(oldpath)
	if err != nil {
		return err
	}
	parent, name, err := splitPath(newpath)
	if err != nil {
		return err
	}
	parentInode, err := e.inner.Walk(parent)
	if err != nil {
		return err
	}
	parentNum, _, err := e.inner.WalkInode(parent)
	if err != nil {
		return err
	}
	if targetInode.IsDir() {
		return fmt.Errorf("permission denied")
	}
	fileType := uint8(vfs.DirFileTypeRegular)
	if targetInode.IsSymlink() {
		fileType = uint8(vfs.DirFileTypeSymlink)
	}
	if err := e.inner.AddDirEntry(parentInode, name, targetNum, fileType); err != nil {
		return err
	}
	targetInode.I_links_count++
	if err := e.inner.WriteInode(targetNum, targetInode); err != nil {
		return err
	}
	parentInode.I_mtime = uint32(time.Now().Unix())
	return e.inner.WriteInode(parentNum, parentInode)
}

func (e *FileSystem) Statfs() *fuse.Statfs_t {
	sb, err := e.inner.Superblock()
	if err != nil {
		return &fuse.Statfs_t{}
	}
	stat := &fuse.Statfs_t{}
	stat.Bsize = uint64(sb.BlockSize())
	stat.Frsize = stat.Bsize
	stat.Blocks = uint64(sb.S_blocks_count_lo)
	stat.Bfree = uint64(sb.S_free_blocks_count_lo)
	stat.Bavail = stat.Bfree
	stat.Files = uint64(sb.S_inodes_count)
	stat.Ffree = uint64(sb.S_free_inodes_count)
	stat.Favail = stat.Ffree
	stat.Namemax = 255
	return stat
}

func (e *FileSystem) Getxattr(path string, name string) (int, []byte) {
	inodeNum, _, err := e.inner.WalkInode(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			return -fuse.ENOENT, nil
		}
		return -fuse.EIO, nil
	}

	val, err := e.inner.GetXAttr(inodeNum, name)
	if err != nil {
		if errors.Is(err, vfs.ErrAttrNotExist) {
			return -fuse.ENOATTR, nil
		}
		return -fuse.EIO, nil
	}

	return 0, val
}

func (e *FileSystem) Setxattr(path string, name string, value []byte, flags int) (res int) {
	e.inner.BeginTransaction()
	defer func() {
		if res != 0 {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				res = -fuse.EIO
			}
		}
	}()

	inodeNum, _, err := e.inner.WalkInode(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}

	if flags != 0 {
		exists := false
		xattrs, err := e.inner.ListXAttrs(inodeNum)
		if err == nil {
			_, exists = xattrs[name]
		}
		if flags == 1 && exists {
			return -fuse.EEXIST
		}
		if flags == 2 && !exists {
			return -fuse.ENOATTR
		}
	}

	err = e.inner.SetXAttr(inodeNum, name, value)
	if err != nil {
		if errors.Is(err, vfs.ErrNoSpace) {
			return -fuse.ENOSPC
		}
		return -fuse.EIO
	}

	return 0
}

func (e *FileSystem) Listxattr(path string, fill func(name string) bool) int {
	inodeNum, _, err := e.inner.WalkInode(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}

	xattrs, err := e.inner.ListXAttrs(inodeNum)
	if err != nil {
		return -fuse.EIO
	}

	for k := range xattrs {
		if !fill(k) {
			break
		}
	}

	return 0
}

func (e *FileSystem) Removexattr(path string, name string) (res int) {
	e.inner.BeginTransaction()
	defer func() {
		if res != 0 {
			e.inner.RollbackTransaction()
		} else {
			if commitErr := e.inner.CommitTransaction(); commitErr != nil {
				res = -fuse.EIO
			}
		}
	}()

	inodeNum, _, err := e.inner.WalkInode(path)
	if err != nil {
		if errors.Is(err, vfs.ErrNotExist) {
			return -fuse.ENOENT
		}
		return -fuse.EIO
	}

	err = e.inner.RemoveXAttr(inodeNum, name)
	if err != nil {
		if errors.Is(err, vfs.ErrAttrNotExist) {
			return -fuse.ENOATTR
		}
		return -fuse.EIO
	}

	return 0
}

func splitPath(path string) (parent, name string, err error) {
	if path == "" || path == "/" {
		return "", "", fmt.Errorf("cannot split root path")
	}
	idx := len(path) - 1
	for idx >= 0 && path[idx] == '/' {
		idx--
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
