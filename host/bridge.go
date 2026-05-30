package host

import (
	"errors"
	"log"
	"strings"

	"github.com/NilayShenai/Werunos/fs"
	"github.com/winfsp/cgofuse/fuse"
)

type openFile struct {
	path string
}

type OrionFS struct {
	fuse.FileSystemBase
	inner      fs.FileSystem
	fileHandles HandleTable[openFile]
	dirHandles  HandleTable[string]
}

func NewOrionFS(filesys fs.FileSystem) *OrionFS {
	return &OrionFS{inner: filesys}
}

func (j *OrionFS) Init() {
<<<<<<< HEAD
	log.Printf("[FUSE] Init() - filesystem type=%s is live, WinFsp handshake complete", j.inner.Type())
=======
	log.Printf("[FUSE] Init() - filesystem is live, FUSE handshake complete")
>>>>>>> fe95509 (made it compatible with mac)
}

func (j *OrionFS) Destroy() {
	j.inner.Destroy()
	log.Printf("[FUSE] Destroy() - filesystem unmounted")
}

func (j *OrionFS) Statfs(path string, stat *fuse.Statfs_t) int {
	fsStat := j.inner.Statfs()
	*stat = *fsStat
	log.Printf("[FUSE] Statfs(%q) OK - bsize=%d blocks=%d bfree=%d",
		path, stat.Bsize, stat.Blocks, stat.Bfree)
	return 0
}

func (j *OrionFS) Getattr(path string, stat *fuse.Stat_t, fh uint64) int {
	node, err := j.inner.Getattr(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("[FUSE] Getattr(%q) → ENOENT", path)
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Getattr(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	nodeToStat(node, stat)
	log.Printf("[FUSE] Getattr(%q) OK - mode=0%o size=%d", path, stat.Mode, stat.Size)
	return 0
}

func (j *OrionFS) Opendir(path string) (int, uint64) {
	fh := j.dirHandles.Store(path)
	log.Printf("[FUSE] Opendir(%q) OK - fh=%d", path, fh)
	return 0, fh
}

func (j *OrionFS) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {

	dirPath, ok := j.dirHandles.Load(fh)
	if !ok {
		log.Printf("[FUSE] Readdir(%q) fh=%d → EBADF", path, fh)
		return -fuse.EBADF
	}

	entries, err := j.inner.Readdir(dirPath)
	if err != nil {
		log.Printf("[FUSE] Readdir(%q) ERROR: %v", dirPath, err)
		return -fuse.EIO
	}

	log.Printf("[FUSE] Readdir(%q) fh=%d - filling %d entries", dirPath, fh, len(entries))
	for _, e := range entries {
		var s fuse.Stat_t
		if node, err := j.inner.Getattr(dirPath + "/" + e.Name); err == nil {
			nodeToStat(node, &s)
		}
		if !fill(e.Name, &s, 0) {
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
	if err := j.inner.Open(path, flags); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("[FUSE] Open(%q) → ENOENT", path)
			return -fuse.ENOENT, 0
		}
		log.Printf("[FUSE] Open(%q) ERROR: %v", path, err)
		return -fuse.EIO, 0
	}
	fh := j.fileHandles.Store(openFile{path: path})
	log.Printf("[FUSE] Open(%q) OK - fh=%d", path, fh)
	return 0, fh
}

func (j *OrionFS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	n, err := j.inner.Read(path, buff, ofst)
	if err != nil {
		log.Printf("[FUSE] Read(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	return n
}

func (j *OrionFS) Release(path string, fh uint64) int {
	log.Printf("[FUSE] Release(%q) fh=%d", path, fh)
	j.fileHandles.Delete(fh)
	if err := j.inner.Release(path); err != nil {
		return -fuse.EIO
	}
	return 0
}

func (j *OrionFS) Readlink(path string) (int, string) {
	target, err := j.inner.Readlink(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Printf("[FUSE] Readlink(%q) → ENOENT", path)
			return -fuse.ENOENT, ""
		}
		log.Printf("[FUSE] Readlink(%q) ERROR: %v", path, err)
		return -fuse.EIO, ""
	}
	log.Printf("[FUSE] Readlink(%q) → %q", path, target)
	return 0, target
}

func (j *OrionFS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	n, err := j.inner.Write(path, buff, ofst)
	if err != nil {
		log.Printf("[FUSE] Write(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	return n
}

func (j *OrionFS) Create(path string, flags int, mode uint32) (int, uint64) {
	if err := j.inner.Create(path, flags, mode); err != nil {
		log.Printf("[FUSE] Create(%q) ERROR: %v", path, err)
		return -fuse.EIO, 0
	}
	fh := j.fileHandles.Store(openFile{path: path})
	log.Printf("[FUSE] Create(%q) OK - fh=%d", path, fh)
	return 0, fh
}

func (j *OrionFS) Mkdir(path string, mode uint32) int {
	if err := j.inner.Mkdir(path, mode); err != nil {
		log.Printf("[FUSE] Mkdir(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Mkdir(%q) OK", path)
	return 0
}

func (j *OrionFS) Unlink(path string) int {
	if err := j.inner.Unlink(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Unlink(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Unlink(%q) OK", path)
	return 0
}

func (j *OrionFS) Rmdir(path string) int {
	if err := j.inner.Rmdir(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Rmdir(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Rmdir(%q) OK", path)
	return 0
}

func (j *OrionFS) Rename(oldpath string, newpath string) int {
	if err := j.inner.Rename(oldpath, newpath); err != nil {
		log.Printf("[FUSE] Rename(%q → %q) ERROR: %v", oldpath, newpath, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Rename(%q → %q) OK", oldpath, newpath)
	return 0
}

func (j *OrionFS) Chmod(path string, mode uint32) int {
	if err := j.inner.Chmod(path, mode); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Chmod(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Chmod(%q) → 0%o", path, mode)
	return 0
}

func (j *OrionFS) Chown(path string, uid uint32, gid uint32) int {
	if err := j.inner.Chown(path, uid, gid); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Chown(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Chown(%q) → uid=%d gid=%d", path, uid, gid)
	return 0
}

func (j *OrionFS) Truncate(path string, size int64, fh uint64) int {
	if err := j.inner.Truncate(path, size); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Truncate(%q) ERROR: %v", path, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Truncate(%q) OK → %d bytes", path, size)
	return 0
}

func (j *OrionFS) Symlink(target string, newpath string) int {
	if err := j.inner.Symlink(target, newpath); err != nil {
		log.Printf("[FUSE] Symlink(%q → %q) ERROR: %v", target, newpath, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Symlink(%q → %q) OK", target, newpath)
	return 0
}

func (j *OrionFS) Link(oldpath string, newpath string) int {
	if err := j.inner.Link(oldpath, newpath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return -fuse.ENOENT
		}
		log.Printf("[FUSE] Link(%q → %q) ERROR: %v", oldpath, newpath, err)
		return -fuse.EIO
	}
	log.Printf("[FUSE] Link(%q → %q) OK", oldpath, newpath)
	return 0
}

func nodeToStat(node fs.NodeInfo, stat *fuse.Stat_t) {
	mode := node.Mode
	if mode == 0 {
		mode = 0664
		if node.IsDir {
			mode |= 0111 | 0x4000
		}
	}
	stat.Mode = mode
	stat.Nlink = uint32(node.Nlink)
	stat.Size = int64(node.Size)
	stat.Atim = fuse.NewTimespec(node.Atime)
	stat.Ctim = fuse.NewTimespec(node.Ctime)
	stat.Mtim = fuse.NewTimespec(node.Mtime)
	if node.Blksize > 0 {
		stat.Blksize = int64(node.Blksize)
		stat.Blocks = int64(node.Blocks * (node.Blksize / 512))
	} else {
		stat.Blksize = 512
		stat.Blocks = int64(node.Blocks)
	}
}

func splitPath(path string) (parent, name string, err error) {
	if path == "" || path == "/" {
		return "", "", errSplitRoot
	}
	idx := strings.LastIndex(path, "/")
	if idx == 0 {
		parent = "/"
	} else {
		parent = path[:idx]
	}
	name = path[idx+1:]
	if name == "" {
		return "", "", errEmptyName
	}
	return parent, name, nil
}

var errSplitRoot = errors.New("cannot split root path")
var errEmptyName = errors.New("empty leaf name")
