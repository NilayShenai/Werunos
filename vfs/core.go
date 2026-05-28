package vfs

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

// ReadWriterAt is the interface used for all device I/O.
// It combines io.ReaderAt and io.WriterAt (which Go does not provide as
// a single pre-defined interface).
type ReadWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

var ErrNotExist = errors.New("no such file or directory")

type FileSystem struct {

	dev ReadWriterAt

	sb *SuperBlock

	Bgds []GroupDescriptor

	BlockSize uint64

	InodeSize uint16

	GroupCount uint32

	DescSize uint16

	dirCache sync.Map
}

func NewFileSystem(device ReadWriterAt) (*FileSystem, error) {
	return &FileSystem{dev: device}, nil
}

func (fs *FileSystem) Superblock() (*SuperBlock, error) {
	if fs.sb == nil {
		return nil, fmt.Errorf("superblock not read yet - call ReadSuperBlock first")
	}
	return fs.sb, nil
}

func (fs *FileSystem) Walk(path string) (*Inode, error) {
	_, inode, err := fs.walk(path, 0)
	return inode, err
}

func (fs *FileSystem) WalkInode(path string) (uint32, *Inode, error) {
	return fs.walk(path, 0)
}

const maxSymlinks = 40

func (fs *FileSystem) walk(path string, symDepth int) (uint32, *Inode, error) {
	if symDepth > maxSymlinks {
		return 0, nil, fmt.Errorf("too many levels of symbolic links")
	}

	curNum := uint32(RootInodeNum)
	cur, err := fs.ReadInode(curNum)
	if err != nil {
		return 0, nil, fmt.Errorf("walk: failed to read root inode: %w", err)
	}

	components := strings.Split(path, "/")

	for _, component := range components {

		if component == "" {
			continue
		}

		if !cur.IsDir() {
			return 0, nil, fmt.Errorf("walk: %q is not a directory", component)
		}

		childInodeNum, err := fs.Lookup(cur, component)
		if err != nil {
			return 0, nil, err
		}

		child, err := fs.ReadInode(childInodeNum)
		if err != nil {
			return 0, nil, fmt.Errorf("walk: failed to read inode %d for %q: %w", childInodeNum, component, err)
		}

		if child.IsSymlink() {
			target, err := fs.ReadSymlink(child)
			if err != nil {
				return 0, nil, fmt.Errorf("walk: failed to read symlink %q: %w", component, err)
			}

			resolvedNum, resolved, err := fs.walk(target, symDepth+1)
			if err != nil {
				return 0, nil, fmt.Errorf("walk: symlink %q → %q: %w", component, target, err)
			}
			childInodeNum = resolvedNum
			child = resolved
		}

		curNum = childInodeNum
		cur = child
	}

	return curNum, cur, nil
}
