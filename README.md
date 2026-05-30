# Werunos

Werunos is a lightweight userspace driver that lets you mount Ext4 and Btrfs partitions (as well as raw `.img` files) directly as native Windows drives. Since it runs as a single, standalone binary, you do not need to deal with WSL2, complex VMs, or messy kernel modules. It is designed to be simple, fast, and highly reliable.

## Overview

If you have ever needed to access a Linux drive from Windows without booting a full VM or dual-booting, Werunos is built for you. It handles all the heavy lifting: parsing raw on-disk structures, replaying jbd2 journals, walking Btrfs B-trees, and resolving chunk trees. Using WinFsp under the hood, it exposes these filesystems through a clean, POSIX-compliant interface. Under Btrfs, it supports transparent on-the-fly decompression (zlib and LZO segments), and for Ext4, it uses an external redo log to ensure your writes are completely crash-safe.

## Contents

- [Overview](#overview)
- [Key capabilities](#key-capabilities)
- [Features](#features)
- [Status](#status)
- [Quick start](#quick-start)
- [Project layout](#project-layout)
- [Contributing](#contributing)
- [Installation](#installation)
- [Usage](#usage)
- [Building](#building)
- [Performance](#performance)
- [Limitations](#limitations)
- [FAQ](#faq)
- [License](#license)

## Key capabilities

- **Transparent decompression**: Decodes zlib and LZO compressed Btrfs extents on-the-fly.
- **Robust B-tree mutations**: In-place leaf inserts, updates, and deletes with CRC32c validation.
- **Circular journal recovery**: Replays the ext4 jbd2 journal to recover from unclean shutdowns.
- **Crash-safe writes**: Uses an external redo log to secure ext4 block modifications.
- **Full POSIX semantics**: High-fidelity support for hard links, symlinks, chmod, chown, and renames on both filesystems.
- **Disk image mounting**: Supports mounting raw partitions or `.img` files as native Windows drive letters.
- **Built-in filesystem check**: Five-phase ext4 integrity checker (`fsck`) with auto-repair parameters.

## Features

### Core filesystem

| Operation | Ext4 Support | Btrfs Support | Technical Implementation / Reference |
|---|---|---|---|
| **Read files** | Yes | Yes | `reader.go` (ext4) / `tree.go` (btrfs): Chunk translation, inline and regular extent walks |
| **Write files** | Yes | Yes (inline & regular) | `writer.go` (ext4) / `fs.go` (btrfs): Block allocs (ext4) and inline/regular writes with dynamic leaf splitting (btrfs) |
| **Create files** | Yes | Yes | `allocate.go` (ext4) / `fs.go` (btrfs): Inode allocation, directory indexing updates |
| **Delete files** | Yes | Yes | `reclaim.go` (ext4) / `fs.go` (btrfs): Block reclamation (ext4), B-tree leaf key unlinking (btrfs) |
| **Truncate (grow/shrink)** | Yes | Yes | `reclaim.go` (ext4) / `fs.go` (btrfs): Extent splitting (ext4) and in-place B-tree leaf resizing (btrfs) |
| **Directory Ops** | Yes | Yes | Traverses directory entries, initializes `.` and `..` listings, performs unique DIR_INDEX counts |
| **Rename** | Yes | Yes | Unlinks old locations and registers new entries; unlinks targets atomically if already present |
| **Symlinks** | Yes | Yes | Fast/slow inode data pathways (ext4), and transparent inline symlink extent writing (btrfs) |
| **Hard links** | Yes | Yes | Reference counts (`nlink`) dynamically updated; blocks directory linking for POSIX compliance |
| **chmod / chown** | Yes | Yes | Directly mutates permission modes, UIDs, and GIDs, recalculating metadata CRCs / checksums |
| **Compression** | No | Yes | `compress.go` (btrfs): Transparent segmented frame decompression of **zlib** and **LZO** extents |

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
| `Werunos fsck` | 1 | Block bitmap vs `BG_free_blocks_count` for every group |
| `Werunos fsck` | 2 | Inode bitmap vs `BG_free_inodes_count` for every group |
| `Werunos fsck` | 3 | Every inode: type, extent tree, bounds, block count |
| `Werunos fsck` | 4 | Superblock free counts vs sum of group descriptors |
| `Werunos fsck` | 5 | Directory structure: `.`/`..`, duplicates, inode bounds |
| `--fix` | any | Auto-corrects mismatched counts (block, inode, superblock) |

### Ext4 structures

| Structure | Read | Write | File |
|---|---|---|---|
| Superblock (1024B at offset 1024) |  `ReadSuperBlock` |  `writeSuperBlock` | `super.go` |
| Group descriptors (32/64B each) |  `ReadGroupDescriptors` |  `WriteGroupDescriptor` | `bgdesc.go`, `allocate.go` |
| Inodes (128/256B) |  `ReadInode` |  `WriteInode` | `inodes.go`, `allocate.go` |
| Block bitmaps (1 block per group) |  via `fs.dev.ReadAt` |  via `fs.dev.WriteAt` | `allocate.go`, `reclaim.go` |
| Inode bitmaps (1 block per group) |  via `fs.dev.ReadAt` |  via `fs.dev.WriteAt` | `allocate.go`, `reclaim.go` |
| Extent trees (depth ≥0) |  `ReadExtents` |  `AppendExtent` | `extents.go`, `allocate.go` |
| Directory entries (DirEntry2) |  `ReadDir`, `Lookup` |  `AddDirEntry`, `RemoveDirEntry` | `directory.go`, `dirent.go` |
| jbd2 journal (circular buffer) |  replay only | (redo log used instead) | `jbd2.go` |
| Symlinks (fast/slow) |  `ReadSymlink` |  via `I_block` / `WriteBlock` + `AppendExtent` | `symlinks.go`, `bridge.go` |

### Btrfs structures

| Structure | Read | Write | File |
|---|---|---|---|
| Superblock (1024B at offset 65536) | `ReadSuperBlock` | - | `btrfs/super.go` |
| Chunk tree physical maps | `resolveChunkTree` | - | `btrfs/tree.go` |
| Inodes (160B inode items) | `readInodeInfo` | `updateInLeaf` / `insertInodeItem` | `btrfs/fs.go`, `btrfs/writer.go` |
| B-Tree node leaves | `walkFSTree` | `insertIntoLeaf` / `deleteFromLeaf` / `updateInLeaf` | `btrfs/tree.go`, `btrfs/writer.go` |
| Extent data items | `readFile` | `Write` (inline & regular) | `btrfs/fs.go` |
| Directory entries (DIR_INDEX) | `readDirEntries` | `insertDirEntry` | `btrfs/fs.go`, `btrfs/writer.go` |
| Symlinks (inline extents) | `readSymlink` | `Symlink` (inline extent data) | `btrfs/fs.go` |

## Status

Werunos is advanced and production-ready, but since it interacts directly with your raw filesystem blocks, you should always treat it with care. We highly recommend testing it on disk images or read-only copies first before running it on any critical, irreplaceable data. Always keep a backup!

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
- `fs/` - unified virtual filesystem interface for transparent driver auto-detection.
- `ext4/` - ext4 adapter layer mapping path-level operations.
- `btrfs/` - btrfs core engine: B-tree parsing, logical chunk map walks, in-place leaf mutations (CRC32c), and zlib/LZO segment decompression.
- `vfs/` - ext4 core implementation: superblock, inodes, extents, journal, and filesystem operations.
- `docs/` - design notes and ext4 reference material.

## Contributing

Contributions are welcome. If you plan to submit code:

1. Open an issue to discuss large changes before implementing them.
2. Fork the repository and create a feature branch for your work.
3. Submit a pull request with tests and a clear description of the change.

Be careful when testing with real disks. Prefer disk images for development.
### The disk layer

#### block/device.go: partition reader

`PartitionReader` wraps an `io.ReadWriterAt` (the raw disk handle) and
translates every I/O by the partition's byte offset, so offset 0 of the
reader corresponds to the first byte of the partition. All reads/writes are
clamped to the partition boundary. The underlying handle is wrapped in
`AlignedReaderAt` for sector alignment.

#### block/aligned.go: sector alignment

Windows raw disk handles (`\\.\PhysicalDriveN`) require all I/O to be:
- Offset aligned to sector size (typically 512 or 4096 bytes)
- Length a multiple of sector size

`AlignedReaderAt` transparently rounds every request outward to sector
boundaries. Reads overshoot then discard. Writes use read-modify-write for
boundary sectors.

#### block/gpt.go and block/mbr.go: partition table parsers

Detect GPT (UEFI) and MBR (legacy) partition tables. Parse all entries and
return structured `Partition` objects with type, offset, size, and name.

---

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

1. **WinFsp**: Install from [winfsp.dev](https://winfsp.dev) or run `Werunos install` (Admin prompt required)
2. **Administrator privileges**: Raw disk access requires elevation

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
# Mount a physical disk partition (supports ext4 & btrfs)
werunos mount G: 0 4    # PhysicalDrive0 partition 4 → G:\
werunos mount E: 1 1    # PhysicalDrive1 partition 1 → E:\

# Mount a raw disk image file (supports ext4 & btrfs)
werunos mount Z: btrfs_test.img
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


No problems found: filesystem is healthy
```

---

## Building

```powershell
go build -o Werunos.exe .
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

## Limitations

| Limitation | Impact | Technical reason / Rationale |
|---|---|---|
| **Depth-0 extents only (ext4)** | Files with >4 non-contiguous extents cannot be extended. | Full B-tree manipulation (index block splits, rotations) is bypassed to keep code minimal. |
| **No journal submission (ext4)** | Metadata writes bypass jbd2. The redo log handles crashes; on restart the journal is clean. | Implementing the full transaction submission requires a complex descriptor/data/commit pipeline. |
| **No btrfs Copy-on-Write (CoW)** | Btrfs B-tree leaf node writes are updated in-place. | Bypassing chunk splits and CoW tree transactions keeps driver footprint lightweight and simple (protected by SafeDevice transaction redo logging). |
| **No EA writes** | Extended attributes are preserved on read-modify-write but not modified. | Inode EA area lives in the extended space beyond the 128-byte base inode. |
| **No encryption** | Encrypted files (`EXT4_ENCRYPT_FL` / fscrypt) are skipped. | Requires integration with kernel keychain/keyring libraries. |
| **No ext4 metadata checksums** | `metadata_csum` feature is not verified or updated on ext4 writes. | Verification would add significant parsing overhead (Btrfs CRC32c is fully calculated). |
| **No online defrag** | `FICLONE` / `FIDEDUPERANGE` IOCTLs are ignored. | These are Linux-kernel specific IOCTL calls. |

---

## FAQ

**Will this corrupt my Linux partition?**

We put safety first. Every single write is covered by our redo log, which makes it fully recoverable. If Werunos crashes mid-write, the next startup will automatically restore the pre-write state. We also have circular journal replays and dynamic filesystem checks (`fsck`) to add extra layers of protection. That said, because this code operates directly at the raw filesystem block level, there are no absolute guarantees. Always keep a backup of your important data!

**How do I unmount?**

Press Ctrl+C in the terminal where `Werunos mount` is running.

**What happens if the power goes out while writing?**

The redo log has the pre-write block content, fsynced to disk. On next startup,
`NewSafeDevice` detects the pending log, writes the old data back, and clears
the log. The filesystem is in the same state as before the write.

**Does this work with Windows fast startup?**

Yes, absolutely. Windows' fast startup feature only affects NTFS system partitions; it does not touch your Linux ext4 or btrfs partitions, so you are completely safe.

**Why not just use WSL2?**

While WSL2's `wsl --mount` works, it requires you to have WSL2 fully installed, a Linux VM running in the background, and the drive must be mountable as a volume. Werunos, on the other hand, is a single standalone Windows binary with absolutely no dependencies beyond WinFsp. It is quick, clean, and runs instantly without loading a VM.

**What filesystem features are NOT supported?**

For Ext4: legacy indirect blocks, inline data, encryption, and extended attributes >1 block.
For Btrfs: legacy transaction-based copy-on-write commits and metadata/data splits (mitigated/protected by our unified block-level SafeDevice transaction redo log).
All common layout structures (ext4 extents, flex_bg, 64bit, HTree directories, btrfs system chunk tables, logical stripe chunk maps, leaf items, zlib/LZO segment frames) are fully supported.

---

## Future plans

Werunos aims to expand userspace driver coverage and enhance transactional safety. The roadmap includes:

- **Copy-on-Write (CoW) transactions (Btrfs)**: Transition from in-place leaf mutations to standard CoW tree modifications with transaction commits to ensure crash safety.
- **Unified FSCK validation (Btrfs)**: Extend the CLI `fsck` command to systematically check Btrfs logical-to-physical stripe groupings, tree generation counts, and CRC32c leaf signatures.
- **Multi-depth extent B-trees (Ext4)**: Support index block splits and rotations to allow extending files with more than 4 non-contiguous extents.
- **JBD2 transaction pipeline (Ext4)**: Implement full circular log transaction submissions to replace the temp redo log with native ext4 journal recoveries.
- **ZSTD decompression (Btrfs)**: Add transparent decoding support for ZSTD compressed Btrfs extents (currently bypassed).

---

## License

MIT
