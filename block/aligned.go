package block

import (
	"fmt"
	"io"
)

type AlignedReaderAt struct {
	inner      ReadWriterAt
	sectorSize int64
}

func NewAlignedReaderAt(inner ReadWriterAt, sectorSize int64) (*AlignedReaderAt, error) {
	if sectorSize <= 0 || sectorSize&(sectorSize-1) != 0 {
		return nil, fmt.Errorf("disk: sector size %d is not a positive power of two", sectorSize)
	}
	return &AlignedReaderAt{
		inner:      inner,
		sectorSize: sectorSize,
	}, nil
}

func (a *AlignedReaderAt) ReadAt(buf []byte, off int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	ss := a.sectorSize

	alignedStart := (off / ss) * ss

	end := off + int64(len(buf))
	alignedEnd := ((end + ss - 1) / ss) * ss

	alignedLen := alignedEnd - alignedStart

	if alignedStart == off && alignedLen == int64(len(buf)) {
		return a.inner.ReadAt(buf, off)
	}

	tmp := make([]byte, alignedLen)
	n, err := a.inner.ReadAt(tmp, alignedStart)

	skip := int(off - alignedStart)

	usable := n - skip
	if usable < 0 {
		usable = 0
	}
	if usable > len(buf) {
		usable = len(buf)
	}

	copied := copy(buf[:usable], tmp[skip:skip+usable])

	if copied == len(buf) {
		return copied, nil
	}

	if err != nil {
		return copied, err
	}

	return copied, io.EOF
}

func (a *AlignedReaderAt) WriteAt(buf []byte, off int64) (int, error) {
	if len(buf) == 0 {
		return 0, nil
	}

	ss := a.sectorSize

	alignedStart := (off / ss) * ss
	end := off + int64(len(buf))
	alignedEnd := ((end + ss - 1) / ss) * ss

	if alignedStart == off && alignedEnd-alignedStart == int64(len(buf)) {
		return a.inner.WriteAt(buf, off)
	}

	tmp := make([]byte, alignedEnd-alignedStart)
	if _, err := a.inner.ReadAt(tmp, alignedStart); err != nil && err != io.EOF {
		return 0, fmt.Errorf("disk: aligned WriteAt read-modify failed: %w", err)
	}

	skip := off - alignedStart
	copy(tmp[skip:], buf)

	if _, err := a.inner.WriteAt(tmp, alignedStart); err != nil {
		return 0, err
	}
	return len(buf), nil
}
