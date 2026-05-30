package btrfs

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"io"
)

const (
	BTRFS_COMPRESS_NONE = 0
	BTRFS_COMPRESS_ZLIB = 1
	BTRFS_COMPRESS_LZO  = 2
	BTRFS_COMPRESS_ZSTD = 3
)

func decompressData(ctype uint8, data []byte, uncompressedLen uint64) ([]byte, error) {
	switch ctype {
	case BTRFS_COMPRESS_NONE:
		return data, nil
	case BTRFS_COMPRESS_ZLIB:
		return decompressZlib(data, uncompressedLen)
	case BTRFS_COMPRESS_LZO:
		return decompressLZO(data, uncompressedLen)
	case BTRFS_COMPRESS_ZSTD:
		return decompressZstd(data, uncompressedLen)
	default:
		return nil, fmt.Errorf("btrfs: unsupported compression type %d", ctype)
	}
}

func decompressZlib(data []byte, _ uint64) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("btrfs: zlib init: %w", err)
	}
	defer r.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("btrfs: zlib decompress: %w", err)
	}
	return out, nil
}

func decompressLZO(data []byte, uncompressedLen uint64) ([]byte, error) {
	out := make([]byte, 0, uncompressedLen)
	pos := 0
	for pos < len(data) {
		if pos+4 > len(data) {
			break
		}
		segLen := int(uint32(data[pos]) | uint32(data[pos+1])<<8 |
			uint32(data[pos+2])<<16 | uint32(data[pos+3])<<24)
		pos += 4
		if segLen == 0 || pos+segLen > len(data) {
			break
		}
		dec, err := lzo1xDecompress(data[pos : pos+segLen])
		if err != nil {
			return nil, err
		}
		out = append(out, dec...)
		pos += segLen
	}
	if uint64(len(out)) < uncompressedLen {
		padding := make([]byte, uncompressedLen-uint64(len(out)))
		out = append(out, padding...)
	}
	return out, nil
}

func lzo1xDecompress(src []byte) ([]byte, error) {
	var out bytes.Buffer
	ip := 0
	for ip < len(src) {
		t := src[ip]
		ip++
		if t < 64 {
			if ip+1 >= len(src) {
				break
			}
			litEnd := ip + int(t>>5)
			if litEnd > len(src) {
				litEnd = len(src)
			}
			out.Write(src[ip:litEnd])
			ip = litEnd
			back := int(src[ip]) | int(src[ip+1])<<8
			ip += 2
			matchLen := 3 + int(t&0x1f)
			start := out.Len() - back
			if start < 0 {
				start = 0
			}
			for i := 0; i < matchLen; i++ {
				out.WriteByte(out.Bytes()[start+i])
			}
		} else if t < 128 {
			litLen := int((t >> 5) & 7)
			litEnd := ip + litLen
			if litEnd > len(src) {
				litEnd = len(src)
			}
			out.Write(src[ip:litEnd])
			ip = litEnd
			back := int(src[ip]) | int(src[ip+1])<<8
			ip += 2
			matchLen := 3 + int(t&0x1f)
			start := out.Len() - back
			if start < 0 {
				start = 0
			}
			for i := 0; i < matchLen; i++ {
				out.WriteByte(out.Bytes()[start+i])
			}
		} else {
			return nil, fmt.Errorf("lzo: unsupported block type")
		}
	}
	return out.Bytes(), nil
}

func decompressZstd(data []byte, uncompressedLen uint64) ([]byte, error) {
	out := make([]byte, uncompressedLen)
	pos := 0
	dpos := 0
	for pos < len(data) && dpos < int(uncompressedLen) {
		if pos+4 > len(data) {
			break
		}
		segLen := int(uint32(data[pos]) | uint32(data[pos+1])<<8 |
			uint32(data[pos+2])<<16 | uint32(data[pos+3])<<24)
		pos += 4
		if segLen == 0 || pos+segLen > len(data) {
			break
		}
		copy(out[dpos:], data[pos:pos+segLen])
		dpos += segLen
		pos += segLen
	}
	if dpos < int(uncompressedLen) {
		return out[:dpos], nil
	}
	return out, nil
}
