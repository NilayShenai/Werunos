package fs

import (
	"errors"
	"time"

	"github.com/winfsp/cgofuse/fuse"
)

var ErrNotExist = errors.New("no such file or directory")

type NodeInfo struct {
	Inode     uint64
	Mode      uint32
	Uid       uint32
	Gid       uint32
	Size      uint64
	Atime     time.Time
	Mtime     time.Time
	Ctime     time.Time
	Nlink     uint64
	Blksize   uint64
	Blocks    uint64
	IsDir     bool
	IsRegular bool
	IsSymlink bool
}

type DirEntry struct {
	Inode    uint64
	Name     string
	FileType uint8
}

const (
	DirFileTypeUnknown  = 0x0
	DirFileTypeRegular  = 0x1
	DirFileTypeDir      = 0x2
	DirFileTypeCharDev  = 0x3
	DirFileTypeBlockDev = 0x4
	DirFileTypeFIFO     = 0x5
	DirFileTypeSocket   = 0x6
	DirFileTypeSymlink  = 0x7
)

var DirFileTypeName = map[uint8]string{
	DirFileTypeUnknown:  "?",
	DirFileTypeRegular:  "f",
	DirFileTypeDir:      "d",
	DirFileTypeCharDev:  "c",
	DirFileTypeBlockDev: "b",
	DirFileTypeFIFO:     "p",
	DirFileTypeSocket:   "s",
	DirFileTypeSymlink:  "l",
}

type FileSystem interface {
	Type() string
	ReadSuperBlock() error
	Destroy()

	Getattr(path string) (NodeInfo, error)
	Readdir(path string) ([]DirEntry, error)
	Readlink(path string) (string, error)
	Open(path string, flags int) error
	Read(path string, buf []byte, ofst int64) (int, error)
	Release(path string) error

	Create(path string, flags int, mode uint32) error
	Write(path string, buf []byte, ofst int64) (int, error)
	Mkdir(path string, mode uint32) error
	Unlink(path string) error
	Rmdir(path string) error
	Rename(oldpath, newpath string) error
	Chmod(path string, mode uint32) error
	Chown(path string, uid, gid uint32) error
	Truncate(path string, size int64) error
	Symlink(target, newpath string) error
	Link(oldpath, newpath string) error

	Getxattr(path string, name string) (int, []byte)
	Setxattr(path string, name string, value []byte, flags int) int
	Listxattr(path string, fill func(name string) bool) int
	Removexattr(path string, name string) int

	BlockSize() uint64
	Statfs() *fuse.Statfs_t
}
