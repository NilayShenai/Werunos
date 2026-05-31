# Changelog v1.8.5

This release implements advanced B-Tree index node splitting, scales Ext4 file storage with multi-depth extent tree writes (Depth > 0), and fully integrates POSIX Extended Attribute (EA) writes on Ext4 partitions.

## New Features

* **Btrfs B-Tree Index Node Splitting**: Fully implemented path-traversal index splits up to arbitrary depths during directory grows. Fixed a critical layout bug by ensuring the correct B-Tree level byte is written at offset 100 in the headers of newly split parent index nodes.
* **Ext4 Multi-depth Extent Trees**: Added intermediate extent indexing, splits, and tree depth scaling (depth > 0) to support files of arbitrary size with non-contiguous block allocations.
* **Ext4 Extended Attribute (EA) Writes**: Created a complete read-modify-write Extended Attribute (xattr) manager supporting in-inode storage (beyond 128-byte base and 28-byte extra sizes) and automated rollover external block allocations (using the I_file_acl block pointer). Fixed a critical layout bug by swapping the serialization order to write the Inode struct first, preventing its last fields from overwriting the in-inode xattr magic.
* **FUSE xattr Adapters**: Wired Getxattr, Setxattr, Listxattr, and Removexattr directly to the host OrionFS adapter, enabling native POSIX ACL and Windows security descriptor support.

## Maintenance and Testing

* **Extended Attribute Test Suite**: Designed and added a complete suite of unit tests in vfs/xattr_test.go utilizing an in-memory block-level mock device to systematically verify in-inode storage, external block storage, prefix compression, and rollover block allocations.
* **Structural Diagnostics**: Integrated an in-memory binary struct checker to map precise field offsets (locating I_extra_isize at exactly offset 128) to guarantee 100% binary compatibility.
* **Automated Code Clean-up**: Designed and executed an advanced Python-based Go source lexer and state-machine to clean trailing whitespace, collapse redundant lines, and strip comments across all 36 Go files, ensuring 100% literal and build-directive safety.
* **Phased Roadmap Documentation**: Overhauled the limitations tables and comparison matrices in the README and website (index.html) to reflect the newly implemented features. Appended a beautiful, styled visual Phased Roadmap component in the web dashboard showing full milestones.
