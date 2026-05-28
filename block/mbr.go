package block

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

const mbrTableOffset = 0x1BE

const mbrSignatureOffset = 0x1FE

var mbrSignature = [2]byte{0x55, 0xAA}

type mbrEntry struct {

	Status uint8

	CHSFirst [3]byte

	Type uint8

	CHSLast [3]byte

	StartLBA uint32

	SectorCount uint32
}

const (
	mbrTypeEmpty         = 0x00
	mbrTypeLinux         = 0x83
	mbrTypeLinuxSwap     = 0x82
	mbrTypeLinuxLVM      = 0x8E
	mbrTypeEFI           = 0xEF
	mbrTypeGPTProtective = 0xEE
	mbrTypeExtended      = 0x05
	mbrTypeExtendedLBA   = 0x0F
	mbrTypeFAT32LBA      = 0x0C
	mbrTypeNTFS          = 0x07
)

func parseMBR(disk io.ReaderAt) ([]Partition, error) {

	sector := make([]byte, 512)
	if _, err := disk.ReadAt(sector, 0); err != nil {
		return nil, fmt.Errorf("mbr: failed to read first sector: %w", err)
	}

	sig := [2]byte{sector[mbrSignatureOffset], sector[mbrSignatureOffset+1]}
	if sig != mbrSignature {
		return nil, fmt.Errorf("mbr: invalid boot signature 0x%02X%02X (expected 0x55AA)", sig[0], sig[1])
	}

	var partitions []Partition
	hasGPTProtective := false

	for i := range 4 {
		offset := mbrTableOffset + i*16
		var e mbrEntry
		if err := binary.Read(bytes.NewReader(sector[offset:offset+16]), binary.LittleEndian, &e); err != nil {
			return nil, fmt.Errorf("mbr: failed to decode entry %d: %w", i, err)
		}

		if e.Type == mbrTypeEmpty || e.SectorCount == 0 {
			continue
		}

		if e.Type == mbrTypeGPTProtective {

			hasGPTProtective = true
			continue
		}

		if e.Type == mbrTypeExtended || e.Type == mbrTypeExtendedLBA {

			continue
		}

		endLBA := uint64(e.StartLBA) + uint64(e.SectorCount) - 1
		partitions = append(partitions, Partition{
			Number:      i + 1,
			Type:        classifyMBRType(e.Type),
			Name:        fmt.Sprintf("Partition %d", i+1),
			Scheme:      SchemeMBR,
			StartLBA:    uint64(e.StartLBA),
			EndLBA:      endLBA,
			SectorSize:  DefaultSectorSize,
			RawTypeByte: e.Type,
		})
	}

	if len(partitions) == 0 && hasGPTProtective {
		return nil, fmt.Errorf("mbr: disk has GPT protective MBR; GPT header may be corrupt")
	}

	return partitions, nil
}

func classifyMBRType(t uint8) PartitionType {
	switch t {
	case mbrTypeLinux:
		return TypeLinuxData
	case mbrTypeLinuxSwap:
		return TypeLinuxSwap
	case mbrTypeLinuxLVM:
		return TypeLinuxLVM
	case mbrTypeEFI:
		return TypeEFISystem
	case mbrTypeFAT32LBA, mbrTypeNTFS:
		return TypeMicrosoftData
	default:
		return TypeUnknown
	}
}
