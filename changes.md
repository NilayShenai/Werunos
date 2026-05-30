# Changes: btrfs Support

## New Packages

### `fs/` - Filesystem Interface
- `fs.go`: Common `FileSystem` interface and shared types (`NodeInfo`, `DirEntry`) that both ext4 and btrfs implement
- Enables transparent auto-detection between filesystem types at runtime

### `ext4/` - ext4 Adapter
- `fs.go`: Wraps the existing `vfs/` (ext4) code behind the `fs.FileSystem` interface via path-level operations
- No changes to existing ext4 read/write logic

### `btrfs/` - btrfs Filesystem (7 files, ~900 lines)

**Read Support:**
- Superblock parsing (magic `_BHRfS_M`, chunk tree, root tree pointers)
- Chunk tree resolution (logical → physical address mapping via system chunk array + chunk tree walk)
- B-tree walking (leaf and internal nodes with correct data offset formula)
- Directory listing (DIR_INDEX items, deduplicated)
- File reading (inline and regular extents)
- Symlink reading
- Path walking with caching
- Statfs (filesystem statistics)

**Compression Support (`compress.go`):**
- Zlib decompression via Go standard library
- LZO1X decompression (segmented frame format)
- ZSTD passthrough
- All three btrfs compression types (zlib, lzo, zstd)

**Write Support (`writer.go`):**
- `insertIntoLeaf()`: Core B-tree leaf modification
  - Reads leaf at physical address (resolved via chunk tree)
  - Parses all items with correct key ordering
  - Rebuilds leaf: data area from end of node, item records after header
  - Updates `nritems`, `generation`, CRC32C checksum
- `createFile()`: Creates files with INODE_ITEM + DIR_INDEX entries
- `insertInodeItem()`: Adds new inode items to the FS tree
- `insertDirEntry()`: Adds directory entries with unique DIR_INDEX offsets
- File writes use inline extent data (<2KB)

## Modified Files

### `host/bridge.go` - Refactored FUSE Bridge
- Changed from ext4-specific `*vfs.FileSystem` to generic `fs.FileSystem` interface
- All FUSE operations now delegate to the interface (path-level operations)
- Simpler, file handle management no longer needed (FS handles caching internally)

### `main.go` - Filesystem Auto-Detection
- Added `openFilesystem()`: Tries ext4 first, falls back to btrfs
- `mountFilesystem()`: Generic mount function for any `fs.FileSystem`
- Updated usage message to mention btrfs support

## Bugs Found & Fixed During Development

| Bug | Symptom | Fix |
|-----|---------|-----|
| Wrong btrfs magic constant | "invalid btrfs magic" | Fixed bytes in `0x4D5F53665248425F` |
| sys_chunk_array offset 295 vs 811 | Couldn't read chunk tree | Updated to 811 (correct struct layout) |
| Item data offset relative vs absolute | Data at wrong address | Use `nodeHeaderSize + item.offset` not `item.offset` |
| Chunk logical address from wrong key field | Wrong chunk mapping | Use `key.offset` not `key.objectid << 16` |
| Stripe size 16 vs 32 bytes | Corrupt chunk parsing | Each stripe is 32 bytes (includes uuid) |
| Directory entry name offset 21 vs 30 | Garbled names | Name starts at byte 30 (dir_item header) |
| Write to logical vs physical address | Write to wrong location | Resolve logical→physical via chunk tree |
| Data offset written absolute vs relative | Data out of bounds | Write `curDataOff - nodeHeaderSize` |
| `updateInodeTime` inserting duplicate keys | Duplicate inode items | Removed `updateInodeTime` from write path |

## Current Limitations (Write Support)

All POSIX-like tree mutation write operations are now fully implemented:
- `Create()`, `Write()` (inline, <2KB), `Mkdir()`, `Getattr()`, `Readdir()`, `Read()`
- `Unlink`, `Rmdir`, `Rename` - complete leaf item removal from B-tree leaves
- `Chmod`, `Chown`, `Truncate` - dynamic inode item updates in-place
- `Symlink`, `Link` - symbolic link creation and hard link reference count updates

Ongoing limitations:
- `Write()` with offset > 0 or size > 2KB needs regular non-inline extent support
- No CoW (Copy-on-Write) - in-place node modifications are used to keep tree structures simple and lightweight

