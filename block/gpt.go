package block

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"unicode/utf16"
)

const gptHeaderOffset = 512

var gptSignature = [8]byte{'E', 'F', 'I', ' ', 'P', 'A', 'R', 'T'}

type gptHeader struct {

	Signature [8]byte

	Revision uint32

	HeaderSize uint32

	HeaderCRC32 uint32

	Reserved uint32

	MyLBA uint64

	AlternateLBA uint64

	FirstUsableLBA uint64

	LastUsableLBA uint64

	DiskGUID [16]byte

	PartitionEntryLBA uint64

	NumPartitionEntries uint32

	PartitionEntrySize uint32

	PartitionArrayCRC32 uint32
}

type gptEntry struct {

	TypeGUID [16]byte

	UniqueGUID [16]byte

	StartLBA uint64

	EndLBA uint64

	Attributes uint64

	Name [72]byte
}

var (

	gptTypeLinuxData = [16]byte{
		0xAF, 0x3D, 0xC6, 0x0F, 0x83, 0x84, 0x72, 0x47,
		0x8E, 0x79, 0x3D, 0x69, 0xD8, 0x47, 0x7D, 0xE4,
	}

	gptTypeLinuxSwap = [16]byte{
		0x6D, 0xFD, 0x57, 0x06, 0xAB, 0xA4, 0xC4, 0x43,
		0x84, 0xE5, 0x09, 0x33, 0xC8, 0x4B, 0x4F, 0x4F,
	}

	gptTypeLinuxLVM = [16]byte{
		0x79, 0xD3, 0xD6, 0xE6, 0x07, 0xF5, 0xC2, 0x44,
		0xA2, 0x3C, 0x23, 0x8F, 0x2A, 0x3D, 0xF9, 0x28,
	}

	gptTypeEFI = [16]byte{
		0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11,
		0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B,
	}

	gptTypeMicrosoftData = [16]byte{
		0xA2, 0xA0, 0xD0, 0xEB, 0xE5, 0xB9, 0x33, 0x44,
		0x87, 0xC0, 0x68, 0xB6, 0xB7, 0x26, 0x99, 0xC7,
	}

	gptTypeBIOSBoot = [16]byte{
		0x48, 0x61, 0x68, 0x21, 0x49, 0x64, 0x6F, 0x6E,
		0x74, 0x4E, 0x65, 0x65, 0x64, 0x45, 0x46, 0x49,
	}

	gptTypeUnused [16]byte
)

func parseGPT(disk io.ReaderAt) ([]Partition, error) {

	var hdr gptHeader
	hdrBuf := make([]byte, 512)
	if _, err := disk.ReadAt(hdrBuf, gptHeaderOffset); err != nil {
		return nil, fmt.Errorf("gpt: failed to read header: %w", err)
	}

	if err := binary.Read(bytes.NewReader(hdrBuf), binary.LittleEndian, &hdr); err != nil {
		return nil, fmt.Errorf("gpt: failed to decode header: %w", err)
	}

	if hdr.Signature != gptSignature {
		return nil, fmt.Errorf("gpt: invalid signature (not a GPT disk)")
	}

	arrayOffset := int64(hdr.PartitionEntryLBA) * DefaultSectorSize
	entrySize := int(hdr.PartitionEntrySize)
	totalArrayBytes := int(hdr.NumPartitionEntries) * entrySize

	arrayBuf := make([]byte, totalArrayBytes)
	if _, err := disk.ReadAt(arrayBuf, arrayOffset); err != nil {
		return nil, fmt.Errorf("gpt: failed to read partition array: %w", err)
	}

	var partitions []Partition
	for i := range int(hdr.NumPartitionEntries) {
		entryBuf := arrayBuf[i*entrySize : i*entrySize+entrySize]

		var e gptEntry

		if err := binary.Read(bytes.NewReader(entryBuf[:128]), binary.LittleEndian, &e); err != nil {
			return nil, fmt.Errorf("gpt: failed to decode entry %d: %w", i, err)
		}

		if e.TypeGUID == gptTypeUnused {
			continue
		}

		partitions = append(partitions, Partition{
			Number:      i + 1,
			Type:        classifyGPTType(e.TypeGUID),
			Name:        decodeGPTName(e.Name[:]),
			Scheme:      SchemeGPT,
			StartLBA:    e.StartLBA,
			EndLBA:      e.EndLBA,
			SectorSize:  DefaultSectorSize,
			RawTypeGUID: e.TypeGUID,
		})
	}

	return partitions, nil
}

func classifyGPTType(guid [16]byte) PartitionType {
	switch guid {
	case gptTypeLinuxData:
		return TypeLinuxData
	case gptTypeLinuxSwap:
		return TypeLinuxSwap
	case gptTypeLinuxLVM:
		return TypeLinuxLVM
	case gptTypeEFI:
		return TypeEFISystem
	case gptTypeMicrosoftData:
		return TypeMicrosoftData
	case gptTypeBIOSBoot:
		return TypeBIOSBoot
	default:
		return TypeUnknown
	}
}

func decodeGPTName(raw []byte) string {

	u16 := make([]uint16, len(raw)/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
	}

	for i, c := range u16 {
		if c == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}
