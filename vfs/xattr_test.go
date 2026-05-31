package vfs

import (
	"encoding/binary"
	"testing"
)

func TestXAttr_SerializationAndPrefixes(t *testing.T) {
	xattrs := map[string][]byte{
		"user.foo":            []byte("bar"),
		"security.selinux":    []byte("enforcing"),
		"system.posix_acl_access": []byte("acl-data"),
	}

	availSpace := 96
	buf, err := serializeXAttrs(xattrs, availSpace, false)
	if err != nil {
		t.Fatalf("failed to serialize xattrs: %v", err)
	}

	rawInode := make([]byte, 256)
	binary.LittleEndian.PutUint16(rawInode[128:130], 28)
	ibodyOffset := 156

	binaryWriteMagic(rawInode[ibodyOffset:])
	copy(rawInode[ibodyOffset+4:], buf)

	mock := &mockDevice{buf: make([]byte, 8192)}
	fs := &FileSystem{
		dev:        mock,
		sb:         &SuperBlock{S_inodes_per_group: 100},
		Bgds:       []GroupDescriptor{{BG_inode_table_lo: 1}},
		BlockSize:  4096,
		InodeSize:  256,
		GroupCount: 1,
	}

	if _, err := mock.WriteAt(rawInode, 4096); err != nil {
		t.Fatalf("failed to write raw inode to mock device: %v", err)
	}

	parsed, err := fs.ListXAttrs(1)
	if err != nil {
		t.Fatalf("failed to list xattrs: %v", err)
	}

	if len(parsed) != 3 {
		t.Errorf("expected 3 attributes, got %d", len(parsed))
	}

	if string(parsed["user.foo"]) != "bar" {
		t.Errorf("expected 'bar', got %q", string(parsed["user.foo"]))
	}

	if string(parsed["security.selinux"]) != "enforcing" {
		t.Errorf("expected 'enforcing', got %q", string(parsed["security.selinux"]))
	}

	if string(parsed["system.posix_acl_access"]) != "acl-data" {
		t.Errorf("expected 'acl-data', got %q", string(parsed["system.posix_acl_access"]))
	}
}

func TestXAttr_GetSetRemove(t *testing.T) {
	mock := &mockDevice{buf: make([]byte, 8192)}
	fs := &FileSystem{
		dev:        mock,
		sb:         &SuperBlock{S_inodes_per_group: 100},
		Bgds:       []GroupDescriptor{{BG_inode_table_lo: 1}},
		BlockSize:  4096,
		InodeSize:  256,
		GroupCount: 1,
	}

	rawInode := make([]byte, 256)
	binary.LittleEndian.PutUint16(rawInode[128:130], 28)
	if _, err := mock.WriteAt(rawInode, 4096); err != nil {
		t.Fatalf("failed to initialize inode: %v", err)
	}

	err := fs.SetXAttr(1, "user.mime", []byte("text/plain"))
	if err != nil {
		t.Fatalf("SetXAttr failed: %v", err)
	}

	val, err := fs.GetXAttr(1, "user.mime")
	if err != nil {
		t.Fatalf("GetXAttr failed: %v", err)
	}
	if string(val) != "text/plain" {
		t.Errorf("expected 'text/plain', got %q", string(val))
	}

	err = fs.SetXAttr(1, "user.mime", []byte("image/png"))
	if err != nil {
		t.Fatalf("Update SetXAttr failed: %v", err)
	}
	val, err = fs.GetXAttr(1, "user.mime")
	if err != nil {
		t.Fatalf("GetXAttr failed after update: %v", err)
	}
	if string(val) != "image/png" {
		t.Errorf("expected 'image/png', got %q", string(val))
	}

	err = fs.RemoveXAttr(1, "user.mime")
	if err != nil {
		t.Fatalf("RemoveXAttr failed: %v", err)
	}

	_, err = fs.GetXAttr(1, "user.mime")
	if err == nil {
		t.Errorf("expected error getting removed attribute, got nil")
	}
}

func binaryWriteMagic(buf []byte) {
	buf[0] = 0x00
	buf[1] = 0x00
	buf[2] = 0x02
	buf[3] = 0xEA
}
