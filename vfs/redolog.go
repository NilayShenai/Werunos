package vfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

const safeHdrSize = 12

type safeEntry struct {
	Offset int64
	Data   []byte
}

type safeLog struct {
	path string
	f    *os.File
	hdr  [safeHdrSize]byte
}

func openSafeLog(deviceID string) (*safeLog, error) {
	dir := os.TempDir()
	if runtime.GOOS == "windows" {
		dir = filepath.Join(os.TempDir(), "werunos-redo")
	} else {
		dir = filepath.Join("/tmp", "werunos-redo")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("safe: mkdir %s: %w", dir, err)
	}

	name := fmt.Sprintf("redo-%s.log", sanitizeDeviceID(deviceID))
	logPath := filepath.Join(dir, name)

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("safe: open log %s: %w", logPath, err)
	}

	return &safeLog{path: logPath, f: f}, nil
}

func (l *safeLog) readPending() ([]safeEntry, error) {
	info, err := l.f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, nil
	}

	data := make([]byte, info.Size())
	if _, err := l.f.ReadAt(data, 0); err != nil {
		return nil, err
	}

	var entries []safeEntry
	pos := 0
	for pos+safeHdrSize <= len(data) {
		off := int64(binary.LittleEndian.Uint64(data[pos : pos+8]))
		length := int(binary.LittleEndian.Uint32(data[pos+8 : pos+12]))
		pos += safeHdrSize
		if pos+length > len(data) {
			break
		}
		b := make([]byte, length)
		copy(b, data[pos:pos+length])
		pos += length
		entries = append(entries, safeEntry{Offset: off, Data: b})
	}
	return entries, nil
}

func (l *safeLog) appendEntry(off int64, data []byte) error {
	binary.LittleEndian.PutUint64(l.hdr[0:8], uint64(off))
	binary.LittleEndian.PutUint32(l.hdr[8:12], uint32(len(data)))

	if _, err := l.f.Write(l.hdr[:]); err != nil {
		return err
	}
	if _, err := l.f.Write(data); err != nil {
		return err
	}
	return l.f.Sync()
}

func (l *safeLog) removeLastEntry(off int64, dataLen int) error {
	entrySize := safeHdrSize + dataLen
	stat, err := l.f.Stat()
	if err != nil {
		return err
	}
	newSize := stat.Size() - int64(entrySize)
	if newSize < 0 {
		newSize = 0
	}
	return l.f.Truncate(newSize)
}

func (l *safeLog) clear() error {
	return l.f.Truncate(0)
}

func (l *safeLog) closeAndRemove() {
	if l.f != nil {
		l.f.Close()
	}
	os.Remove(l.path)
}

type SafeDevice struct {
	inner ReadWriterAt
	log   *safeLog
}

func NewSafeDevice(inner ReadWriterAt, deviceID string) (*SafeDevice, error) {
	l, err := openSafeLog(deviceID)
	if err != nil {
		return nil, err
	}

	entries, err := l.readPending()
	if err != nil {
		l.closeAndRemove()
		return nil, fmt.Errorf("safe: reading pending log: %w", err)
	}

	if len(entries) > 0 {

		for _, e := range entries {
			if _, err := inner.WriteAt(e.Data, e.Offset); err != nil {
				l.closeAndRemove()
				return nil, fmt.Errorf("safe: recovery write at offset %d failed: %w", e.Offset, err)
			}
		}

		if err := l.clear(); err != nil {
			l.closeAndRemove()
			return nil, fmt.Errorf("safe: clearing log after recovery: %w", err)
		}
	}

	return &SafeDevice{inner: inner, log: l}, nil
}

func (d *SafeDevice) ReadAt(p []byte, off int64) (int, error) {
	return d.inner.ReadAt(p, off)
}

func (d *SafeDevice) WriteAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	old := make([]byte, len(p))
	n, err := d.inner.ReadAt(old, off)
	if err != nil && err != io.EOF {
		return 0, fmt.Errorf("safe: pre-read at %d: %w", off, err)
	}
	old = old[:n]

	if err := d.log.appendEntry(off, old); err != nil {
		return 0, fmt.Errorf("safe: log append at %d: %w", off, err)
	}

	written, err := d.inner.WriteAt(p, off)
	if err != nil {
		return written, err
	}

	if cerr := d.log.removeLastEntry(off, len(old)); cerr != nil {
		_ = cerr
	}

	return written, nil
}

func (d *SafeDevice) Close() error {
	d.log.closeAndRemove()
	return nil
}

func sanitizeDeviceID(path string) string {
	b := make([]byte, len(path))
	for i := 0; i < len(path); i++ {
		c := path[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			b[i] = c
		} else {
			b[i] = '_'
		}
	}
	return string(b)
}
