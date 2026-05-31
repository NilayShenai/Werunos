package block

import (
	"fmt"
	"io"
)

type ReadWriterAt interface {
	io.ReaderAt
	io.WriterAt
}

type PartitionScheme int

const (
	SchemeUnknown PartitionScheme = iota
	SchemeGPT
	SchemeMBR
)

func (s PartitionScheme) String() string {
	switch s {
	case SchemeGPT:
		return "GPT"
	case SchemeMBR:
		return "MBR"
	default:
		return "Unknown"
	}
}

type PartitionType int

const (
	TypeUnknown       PartitionType = iota
	TypeLinuxData
	TypeLinuxSwap
	TypeLinuxLVM
	TypeEFISystem
	TypeMicrosoftData
	TypeBIOSBoot
)

func (t PartitionType) String() string {
	switch t {
	case TypeLinuxData:
		return "Linux filesystem"
	case TypeLinuxSwap:
		return "Linux swap"
	case TypeLinuxLVM:
		return "Linux LVM"
	case TypeEFISystem:
		return "EFI System"
	case TypeMicrosoftData:
		return "Microsoft basic data"
	case TypeBIOSBoot:
		return "BIOS boot"
	default:
		return "Unknown"
	}
}

const DefaultSectorSize = 512

type Partition struct {

	Number int

	Type PartitionType

	Name string

	Scheme PartitionScheme

	StartLBA uint64

	EndLBA uint64

	SectorSize uint64

	RawTypeGUID [16]byte

	RawTypeByte uint8
}

func (p *Partition) StartOffset() int64 {
	return int64(p.StartLBA * p.SectorSize)
}

func (p *Partition) ByteSize() int64 {
	return int64((p.EndLBA - p.StartLBA + 1) * p.SectorSize)
}

type PartitionReader struct {
	disk        ReadWriterAt
	startOffset int64
	size        int64
}

func NewPartitionReader(disk ReadWriterAt, p *Partition) *PartitionReader {
	sectorSize := int64(p.SectorSize)
	if sectorSize == 0 {
		sectorSize = DefaultSectorSize
	}

	aligned, err := NewAlignedReaderAt(disk, sectorSize)
	if err != nil {

		return &PartitionReader{
			disk:        disk,
			startOffset: p.StartOffset(),
			size:        p.ByteSize(),
		}
	}

	return &PartitionReader{
		disk:        aligned,
		startOffset: p.StartOffset(),
		size:        p.ByteSize(),
	}
}

func (pr *PartitionReader) ReadAt(buf []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("disk: negative read offset %d", off)
	}
	if off >= pr.size {
		return 0, fmt.Errorf(
			"disk: read offset %d is beyond partition end (%d bytes)",
			off, pr.size,
		)
	}

	maxRead := pr.size - off
	if int64(len(buf)) > maxRead {
		buf = buf[:maxRead]
	}
	return pr.disk.ReadAt(buf, pr.startOffset+off)
}

func (pr *PartitionReader) WriteAt(buf []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("disk: negative write offset %d", off)
	}
	if off >= pr.size {
		return 0, fmt.Errorf(
			"disk: write offset %d is beyond partition end (%d bytes)",
			off, pr.size,
		)
	}
	maxWrite := pr.size - off
	if int64(len(buf)) > maxWrite {
		buf = buf[:maxWrite]
	}
	return pr.disk.WriteAt(buf, pr.startOffset+off)
}

func ProbePartitions(disk io.ReaderAt) (PartitionScheme, []Partition, error) {

	parts, err := parseGPT(disk)
	if err == nil {
		return SchemeGPT, parts, nil
	}

	parts, err = parseMBR(disk)
	if err == nil {
		return SchemeMBR, parts, nil
	}

	return SchemeUnknown, nil, fmt.Errorf("disk: no recognisable partition table (tried GPT and MBR)")
}
