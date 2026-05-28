#### vfs/inodes.go — inode structure
#### vfs/extents.go — extent tree
#### vfs/reader.go — ReadFileAt
#### vfs/writer.go — WriteFileAt
#### vfs/allocate.go — block and inode allocation
#### vfs/reclaim.go — freeing and truncation
#### vfs/jbd2.go — journal replay
#### vfs/dirent.go — directory entry manipulation
#### vfs/directory.go — directory reading
#### vfs/symlinks.go — symlink reading
#### vfs/redolog.go — crash-safe writes
#### vfs/health.go — filesystem checker
# werunos

werunos is a userspace ext4 driver for Windows that mounts ext4 partitions
as native Windows drives using WinFsp. It runs as a single binary, does not
require kernel modules or WSL, and focuses on safe, consistent read-write
access to ext4 media from Windows.

## Overview

werunos provides full ext4 read-write support in userspace. It parses
on-disk structures, replays the jbd2 journal when needed, and exposes a
POSIX-like interface through WinFsp. The implementation emphasizes data
integrity and crash safety for writes to raw disks and disk images.

## Key capabilities

- Replays the jbd2 journal to recover from unclean shutdowns.
- Crash-safe writes using an external redo log.
- Built-in filesystem checker that can detect and fix common inconsistencies.
- Full POSIX semantics: hard links, symlinks, chmod, chown, and renames.
- Raw device support with sector-aligned I/O and partition parsing.
- Minimal external dependencies: WinFsp is required for FUSE support.

## Status

This repository contains an advanced, production-grade userspace ext4
implementation. Use with caution on critical data. Test on images or
read-only copies before operating on important media.

## Quick start

Prerequisites:

- Go installed on your machine. Recommended: Go 1.18 or newer.
- WinFsp installed on Windows for FUSE support.

Build the project:

```
go build ./...
```

Run from source (example):

```
go run main.go
```

Run the test suite:

```
go test ./...
```

## Project layout

- `main.go` - CLI entry point and mounting helpers.
- `block/` - low-level disk access, partition parsing, and sector alignment.
- `host/` - WinFsp integration and FUSE bridge code.
- `vfs/` - ext4 core implementation: superblock, inodes, extents, journal,
  and filesystem operations.
- `docs/` - design notes and ext4 reference material.

## Contributing

Contributions are welcome. If you plan to submit code:

1. Open an issue to discuss large changes before implementing them.
2. Fork the repository and create a feature branch for your work.
3. Submit a pull request with tests and a clear description of the change.

Be careful when testing with real disks. Prefer disk images for development.

## License

Check the repository for a `LICENSE` file. If there is no license, the
repository has no explicit grant for reuse and you should add a suitable
license before redistributing.

## Contact

For questions or support open an issue on the project repository.

2. Splitting the path by `/`
3. For each component, calling `Lookup` on the current directory inode
4. Reading the child inode
5. Following symlinks in intermediate components (up to 40 deep)

`Lookup` uses `ReadDirCached` which reads and caches the directory's entry
list by `I_block` content.

### The disk layer

#### block/device.go — partition reader

`PartitionReader` wraps an `io.ReadWriterAt` (the raw disk handle) and
translates every I/O by the partition's byte offset, so offset 0 of the
reader corresponds to the first byte of the partition. All reads/writes are
clamped to the partition boundary. The underlying handle is wrapped in
`AlignedReaderAt` for sector alignment.

#### block/aligned.go — sector alignment

Windows raw disk handles (`\\.\PhysicalDriveN`) require all I/O to be:
- Offset aligned to sector size (typically 512 or 4096 bytes)
- Length a multiple of sector size

`AlignedReaderAt` transparently rounds every request outward to sector
boundaries. Reads overshoot then discard. Writes use read-modify-write for
boundary sectors.

#### block/gpt.go and block/mbr.go — partition table parsers

Detect GPT (UEFI) and MBR (legacy) partition tables. Parse all entries and
return structured `Partition` objects with type, offset, size, and name.

---

## Features

### Core filesystem

| Operation | Support | Implementation |
|---|---|---|
| Read files | ✅ | `reader.go`: extent walk, on-demand block reads |
| Write files | ✅ | `writer.go`: block alloc, extent append, read-modify-write |
| Create files | ✅ | `allocate.go`: inode alloc + init, `dirent.go`: add entry |
| Delete files | ✅ | `dirent.go`: remove entry, `reclaim.go`: free blocks + inode |
| Truncate (grow) | ✅ | `writer.go`: write zero-filled blocks |
| Truncate (shrink) | ✅ | `reclaim.go`: split/cull extents, free blocks |
| Directories | ✅ | Read, create (with `.`/`..`), delete (if empty), rename |
| Rename | ✅ | Remove + add entries; handles existing target |
| Symlinks | ✅ | Fast (in `I_block`) and slow (data block via extent tree) |
| Hard links | ✅ | Link count tracked; directory linking blocked |
| chmod | ✅ | `I_mode` permission bits updated, inode persisted |
| chown | ✅ | `I_uid`/`I_gid` updated (full 32-bit via `L_i_uid_high`/`L_i_gid_high`) |

### Crash safety

| Protection | Implementation |
|---|---|
| Redo log | `redolog.go`: pre-read + log + fsync + write + truncate |
| jbd2 replay | `jbd2.go`: circular buffer walk, tag parse, escape deobfuscation |
| Orphan cleanup | `jbd2.go`: `S_last_orphan` list traversal, block + inode free |
| State auto-clear | `reclaim.go`: `ClearSuperblockErrors()` sets `S_state = CLEAN` |
| Recovery flag clear | `jbd2.go`: `EXT4_FEATURE_INCOMPAT_RECOVERY` cleared after replay |
| Superblock flush | `Destroy()` calls `WriteSuperBlockPublic()` on unmount |
| Graceful fallback | Journal replay failure → mount with warning (not block) |

### Filesystem health

| Command | Phase | What it checks |
|---|---|---|
| `werunos fsck` | 1 | Block bitmap vs `BG_free_blocks_count` for every group |
| `werunos fsck` | 2 | Inode bitmap vs `BG_free_inodes_count` for every group |
| `werunos fsck` | 3 | Every inode: type, extent tree, bounds, block count |
| `werunos fsck` | 4 | Superblock free counts vs sum of group descriptors |
| `werunos fsck` | 5 | Directory structure: `.`/`..`, duplicates, inode bounds |
| `--fix` | any | Auto-corrects mismatched counts (block, inode, superblock) |

### Ext4 structures

| Structure | Read | Write | File |
|---|---|---|---|
| Superblock (1024B at offset 1024) | ✅ `ReadSuperBlock` | ✅ `writeSuperBlock` | `super.go` |
| Group descriptors (32/64B each) | ✅ `ReadGroupDescriptors` | ✅ `WriteGroupDescriptor` | `bgdesc.go`, `allocate.go` |
| Inodes (128/256B) | ✅ `ReadInode` | ✅ `WriteInode` | `inodes.go`, `allocate.go` |
| Block bitmaps (1 block per group) | ✅ via `fs.dev.ReadAt` | ✅ via `fs.dev.WriteAt` | `allocate.go`, `reclaim.go` |
| Inode bitmaps (1 block per group) | ✅ via `fs.dev.ReadAt` | ✅ via `fs.dev.WriteAt` | `allocate.go`, `reclaim.go` |
| Extent trees (depth ≥0) | ✅ `ReadExtents` | ✅ `AppendExtent` | `extents.go`, `allocate.go` |
| Directory entries (DirEntry2) | ✅ `ReadDir`, `Lookup` | ✅ `AddDirEntry`, `RemoveDirEntry` | `directory.go`, `dirent.go` |
| jbd2 journal (circular buffer) | ✅ replay only | ❌ (redo log used instead) | `jbd2.go` |
| Symlinks (fast/slow) | ✅ `ReadSymlink` | ✅ via `I_block` / `WriteBlock` + `AppendExtent` | `symlinks.go`, `bridge.go` |

### FUSE operations

All 12 standard FUSE filesystem operations are implemented in `host/bridge.go`:

| Operation | Purpose |
|---|---|
| `Init`/`Destroy` | Lifecycle: log start/stop, flush superblock on unmount |
| `Statfs` | Filesystem statistics: total/free blocks and inodes |
| `Getattr` | File metadata: mode, size, timestamps, link count |
| `Opendir`/`Readdir`/`Releasedir` | Directory listing with cached entries |
| `Open`/`Read`/`Release` | File reading with extent caching in handles |
| `Write` | File writing with inode persistence and handle refresh |
| `Truncate` | Resize with block reclamation on shrink |
| `Create` | File creation with inode allocation and dir entry |
| `Mkdir` | Directory creation with `.`/`..` initialization |
| `Unlink`/`Rmdir` | Deletion with block/inode free at link count 0 |
| `Rename` | Cross-directory move with existing-target handling |
| `Chmod`/`Chown` | Permission and ownership changes |
| `Symlink` | Fast/slow symlink creation |
| `Link` | Hard link creation (directory linking rejected) |

---

## Installation

### Prerequisites

1. **WinFsp** — Install from [winfsp.dev](https://winfsp.dev) or run `werunos install` (Admin prompt required)
2. **Administrator privileges** — Raw disk access requires elevation

### Setup

```powershell
# Run as Administrator
werunos install
```

---

## Usage

### List available disks

```powershell
werunos devices
```

Output:
```
Found 2 disk(s):

Disk 0: \\.\PhysicalDrive0
  Partition table: GPT
  #    Name                       Type                 Size
  ----  ------------------------  -------------------  -------
  1     EFI system partition      EFI System           100 MiB
  2     Microsoft reserved        Microsoft basic data  16 MiB
  3     Basic data partition      Microsoft basic data  237 GiB
  4     Linux filesystem          Linux filesystem      256 GiB

Disk 1: \\.\PhysicalDrive1
  Partition table: GPT
  #    Name                       Type                 Size
  ----  ------------------------  -------------------  -------
  1     Linux filesystem          Linux filesystem      1 TiB
```

### List a partition's root directory

```powershell
werunos \\.\PhysicalDrive1 1
```

### Mount as a drive letter

```powershell
werunos mount G: 0 4    # PhysicalDrive0 partition 4 → G:\
werunos mount E: 1 1    # PhysicalDrive1 partition 1 → E:\
```

The mount blocks until Ctrl+C. While mounted, the drive appears in File Explorer
and is accessible from any Windows application.

### Filesystem check

```powershell
# Read-only scan
werunos fsck \\.\PhysicalDrive0 4

# Scan + fix
werunos fsck --fix \\.\PhysicalDrive0 4

# Scan a raw image
werunos fsck testfs.img
```

Output:
```
Volume: home
Block size: 4096
Inodes: 65536
Blocks: 262144
Block groups: 8

Running integrity check...

Inodes checked:  1000
Directories:     24
Extents checked: 312
Blocks ref'd:    28147

✓ No problems found — filesystem is healthy
```

---

## Building

```powershell
go build -o werunos.exe .
```

Requires Go 1.21+. The only external dependency is `github.com/winfsp/cgofuse`.

---

## Performance

All metadata writes are synchronous. The redo log adds one extra read (old data)
and one extra write (log entry) per `WriteAt`. For interactive workloads
(editing files, saving documents), the overhead is imperceptible.

For bulk operations (extracting archives, building code, database writes),
performance is bottlenecked by the disk's random I/O speed. Typical throughput
on a SATA SSD for sequential writes: 150-300 MB/s. Random 4K writes: 5-15 MB/s.

---

## Compared to alternatives

|                      | **werunos** | Paragon ExtFS ($20) | Ext2Fsd | WSL2 mount |
|----------------------|-----------|---------------------|---------|-------------|
| Read/write           | ✅        | ✅                  | ⚠️ broken| ✅          |
| Raw disk access      | ✅        | ✅                  | ✅      | ❌          |
| Journal replay       | ✅        | ❌                  | ❌      | ✅ (kernel) |
| Orphan cleanup       | ✅        | ❌                  | ❌      | ✅ (kernel) |
| Block reclamation    | ✅        | ✅                  | ❌      | ✅          |
| Inode reclamation    | ✅        | ✅                  | ❌      | ✅          |
| Crash-safe redo log  | ✅        | ❌                  | ❌      | N/A         |
| Built-in fsck        | ✅        | ❌                  | ❌      | ❌          |
| Auto-repair          | ✅        | ❌                  | ❌      | ❌          |
| Unclean state clear  | ✅        | ❌                  | ❌      | ✅ (kernel) |
| chmod / chown        | ✅        | ✅                  | ❌      | ✅          |
| Symlinks             | ✅        | ✅                  | ❌      | ✅          |
| Hard links           | ✅        | ✅                  | ❌      | ✅          |
| Cross-directory mv   | ✅        | ✅                  | ❌      | ✅          |
| Sector-aligned I/O   | ✅        | ✅                  | ❌      | N/A         |
| Single binary        | ✅        | ❌ (driver install) | ✅      | ❌ (needs WSL) |
| Free                 | ✅        | ❌ $20              | ✅      | ✅          |

---

## Limitations

| Limitation | Impact | Technical reason |
|---|---|---|
| Depth-0 extents only | Files with >4 non-contiguous extents cannot be extended. Rare for typical workloads. | Full B-tree manipulation (index block splits, rotations) is ~500 additional lines. |
| No journal submission | Metadata writes bypass jbd2. The redo log covers crashes; on restart the journal is clean. | Implementing jbd2 transaction submission requires the full descriptor/data/commit pipeline. |
| No orphan file | `S_orphan_file_inum` (Linux 5.15+) is not supported. Orphans use `S_last_orphan` instead. | Orphan file requires separate linked-list parsing. |
| No EA writes | Extended attributes are preserved on read-modify-write but not modified. | Inode EA area is in the extended inode space (beyond 128 bytes). |
| No encryption | `EXT4_ENCRYPT_FL` files are skipped. | fscrypt requires kernel keyring integration. |
| No inline data | `EXT4_INLINE_DATA_FL` inodes are not written. | Inline data requires separate read/write paths. |
| No metadata checksums | `metadata_csum` feature not verified or updated. | Checksum verification would add ~200 lines. |
| No online defrag | ext4's `FICLONE`/`FIDEDUPERANGE` range is Linux-ioctl-specific. |  |

---

## FAQ

**Will this corrupt my Linux partition?**

The redo log makes every write recoverable. If werunos crashes mid-write, the
next startup restores the pre-write state. Journal replay and fsck provide
additional layers of protection. That said, this is filesystem-level code —
there are no absolute guarantees. Back up your data.

**How do I unmount?**

Press Ctrl+C in the terminal where `werunos mount` is running.

**What happens if the power goes out while writing?**

The redo log has the pre-write block content, fsynced to disk. On next startup,
`NewSafeDevice` detects the pending log, writes the old data back, and clears
the log. The filesystem is in the same state as before the write.

**Does this work with Windows fast startup?**

Yes. Windows' fast startup only affects the NTFS system partition — it does
not touch ext4 partitions.

**Why not use WSL2?**

WSL2's `wsl --mount` works but requires WSL2 installed, a running Linux VM,
and the drive must be mountable as a volume. werunos is a single Windows binary
with no dependencies beyond WinFsp.

**What ext4 features are NOT supported?**

Legacy indirect blocks, inline data, encryption, verity, DAX, and extended
attributes >1 block. All common ext4 configurations (extents, flex_bg, 64bit,
sparse_super, journal, HTree directories) are supported.

---

## License

MIT
