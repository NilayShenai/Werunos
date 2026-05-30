package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/NilayShenai/Werunos/block"
	"github.com/NilayShenai/Werunos/btrfs"
	"github.com/NilayShenai/Werunos/ext4"
	"github.com/NilayShenai/Werunos/fs"
	"github.com/NilayShenai/Werunos/host"
	"github.com/NilayShenai/Werunos/vfs"
	"github.com/winfsp/cgofuse/fuse"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("%v", err)
	}
}

func printHelp() {
	fmt.Print(
		"Werunos - Userspace Ext4 & Btrfs Driver for Windows\n\n" +
			"Usage:\n" +
			"  werunos install                          install WinFsp (requires Admin)\n" +
			"  werunos devices                          list physical disks\n" +
			"  werunos fsck [--fix] <device> [<part>]   check/repair ext4 filesystem\n" +
			"  werunos <device>                         list partitions on device or image\n" +
			"  werunos <device> <partNum>               read root dir of partition\n" +
			"  werunos mount <letter> <disk> <partNum>  mount partition as drive letter\n" +
			"  werunos -h, --help                       show this help info\n\n" +
			"Supported Filesystems:\n" +
			"  ext4, btrfs (both read-write)\n",
	)
}

func run() error {

	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-h", "--help", "help", "/?":
			printHelp()
			return nil
		case "devices":
			return runDevices()
		case "install":
			return runInstall()
		case "mount":
			return runMount()
		}
	}

	switch {
	case len(os.Args) == 2 && os.Args[1] == "fsck":
		return runFsck("testfs.img", false)
	case len(os.Args) == 3 && os.Args[1] == "fsck":
		if os.Args[2] == "--fix" {
			return fmt.Errorf("usage: werunos fsck [--fix] <device> [<partNum>]")
		}
		return runFsck(os.Args[2], false)
	case len(os.Args) == 4 && os.Args[1] == "fsck" && os.Args[2] == "--fix":
		return runFsck(os.Args[3], true)
	case len(os.Args) == 4 && os.Args[1] == "fsck":
		n, err := strconv.Atoi(os.Args[3])
		if err != nil {
			return fmt.Errorf("partition number must be an integer, got %q", os.Args[3])
		}
		return runFsckPartition(os.Args[2], n, false)
	case len(os.Args) == 5 && os.Args[1] == "fsck" && os.Args[2] == "--fix":
		n, err := strconv.Atoi(os.Args[4])
		if err != nil {
			return fmt.Errorf("partition number must be an integer, got %q", os.Args[4])
		}
		return runFsckPartition(os.Args[3], n, true)

	case len(os.Args) == 1:
		return runImageMode()
	case len(os.Args) == 2:
		return runListPartitions(os.Args[1])
	case len(os.Args) == 3:
		n, err := strconv.Atoi(os.Args[2])
		if err != nil {
			return fmt.Errorf("partition number must be an integer, got %q", os.Args[2])
		}
		return runReadPartition(os.Args[1], n)
	default:
		return fmt.Errorf(
			"usage:\n" +
				"  werunos install                          install WinFsp (requires Admin)\n" +
				"  werunos devices                          list physical disks\n" +
				"  werunos fsck [--fix] <device> [<part>]   check/repair ext4 filesystem\n" +
				"  werunos <device>                         list partitions on device\n" +
				"  werunos <device> <partNum>               read root dir of partition\n" +
				"  werunos mount <letter> <disk> <partNum>  mount partition as drive letter\n" +
				"  (supports ext4 and btrfs filesystems)\n",
		)
	}
}

func openFilesystem(dev vfs.ReadWriterAt) (fs.FileSystem, error) {
	ext4fs := ext4.New(dev)
	if err := ext4fs.ReadSuperBlock(); err == nil {
		return ext4fs, nil
	}

	btrfsfs := btrfs.New(dev)
	if err := btrfsfs.ReadSuperBlock(); err == nil {
		return btrfsfs, nil
	}

	return nil, fmt.Errorf("unknown filesystem (not ext4 or btrfs)")
}

func mountFilesystem(dev fs.FileSystem, mountPoint string, volName string) error {
	if volName == "" {
		volName = dev.Type()
	}
	volName = sanitizeVolName(volName)

	hostFS := host.NewOrionFS(dev)
	fuseSrv := fuse.NewFileSystemHost(hostFS)
	mountArgs := []string{"-o", fmt.Sprintf("uid=-1,gid=-1,volname=%s", volName)}
	fmt.Printf("Mounting %s at %s … (press Ctrl+C or right-click → Eject to unmount)\n", volName, mountPoint)

	ok := fuseSrv.Mount(mountPoint, mountArgs)
	if !ok {
		return fmt.Errorf("mount failed - ensure WinFsp is installed")
	}

	fmt.Println("Unmounted successfully.")
	return nil
}

func runDevices() error {
	disks, err := block.EnumerateDisks()
	if err != nil {
		return fmt.Errorf("failed to enumerate disks: %w", err)
	}

	if len(disks) == 0 {
		fmt.Println("No physical disks found.")
		fmt.Println("On Windows, ensure you are running as Administrator.")
		return nil
	}

	fmt.Printf("Found %d disk(s):\n\n", len(disks))

	for di, diskPath := range disks {
		fmt.Printf("Disk %d: %s\n", di, diskPath)

		f, err := os.Open(diskPath)
		if err != nil {
			fmt.Printf("  (could not open: %v)\n\n", err)
			continue
		}

		scheme, partitions, err := block.ProbePartitions(f)
		f.Close()
		if err != nil {
			fmt.Printf("  (could not read partition table: %v)\n\n", err)
			continue
		}

		fmt.Printf("  Partition table: %s\n", scheme)
		fmt.Printf("  %-4s  %-24s  %-18s  %s\n", "#", "Name", "Type", "Size")
		fmt.Printf("  %-4s  %-24s  %-18s  %s\n",
			"----", "------------------------", "------------------", "--------")

		for _, p := range partitions {
			sizeMiB := p.ByteSize() / (1024 * 1024)
			fmt.Printf("  %-4d  %-24s  %-18s  %d MiB\n",
				p.Number, truncate(p.Name, 24), p.Type, sizeMiB)
		}
		fmt.Println()
	}

	fmt.Println("To mount a partition:")
	fmt.Println("  werunos mount <letter> <diskNum> <partNum>")
	fmt.Println("  Example: werunos mount G: 0 1")
	return nil
}

func runMount() error {
	args := os.Args
	if len(args) < 4 || len(args) > 5 {
		return fmt.Errorf(
			"usage: werunos mount <letter> <diskNum|imagePath> [<partNum>]\n" +
				"  example: werunos mount G: 0 1              (physical disk partition)\n" +
				"  example: werunos mount G: testfs.img       (raw ext4/btrfs image file)\n",
		)
	}

	mountPoint := args[2]
	if len(mountPoint) == 1 && mountPoint[0] >= 'A' && mountPoint[0] <= 'Z' ||
		len(mountPoint) == 1 && mountPoint[0] >= 'a' && mountPoint[0] <= 'z' {
		mountPoint = strings.ToUpper(mountPoint) + ":"
	}

	if len(args) == 4 {
		return runMountImage(mountPoint, args[3])
	}

	diskNum, err := strconv.Atoi(args[3])
	if err != nil {
		return runMountImage(mountPoint, args[3])
	}

	partNum, err := strconv.Atoi(args[4])
	if err != nil {
		return fmt.Errorf("partNum must be an integer, got %q", args[4])
	}

	diskPath := fmt.Sprintf(`\\.\PhysicalDrive%d`, diskNum)
	f, err := os.OpenFile(diskPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf(
			"failed to open disk %q: %w\nEnsure werunos is running as Administrator.",
			diskPath, err,
		)
	}
	defer f.Close()

	fmt.Printf("Opened disk: %s\n", diskPath)

	scheme, partitions, err := block.ProbePartitions(f)
	if err != nil {
		return fmt.Errorf("failed to read partition table on %q: %w", diskPath, err)
	}
	fmt.Printf("Partition table: %s\n", scheme)

	var target *block.Partition
	for i := range partitions {
		if partitions[i].Number == partNum {
			target = &partitions[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("partition %d not found on %s", partNum, diskPath)
	}

	if target.Type != block.TypeLinuxData {
		return fmt.Errorf("partition %d is %q - expected Linux filesystem", partNum, target.Type)
	}

	fmt.Printf("Partition %d: %s, offset %d bytes, size %d MiB\n",
		target.Number, target.Name,
		target.StartOffset(), target.ByteSize()/(1024*1024),
	)

	pr := block.NewPartitionReader(f, target)
	deviceID := fmt.Sprintf("%s_p%d", diskPath, partNum)

	safeDev, safeErr := vfs.NewSafeDevice(pr, deviceID)
	if safeErr != nil {
		return fmt.Errorf("failed to initialize safe device: %w", safeErr)
	}
	defer safeDev.Close()

	filesys, err := openFilesystem(safeDev)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	fmt.Printf("Detected filesystem: %s\n", filesys.Type())
	return mountFilesystem(filesys, mountPoint, filesys.Type())
}

func runMountImage(mountPoint, imagePath string) error {
	f, err := os.OpenFile(imagePath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open image %q: %w", imagePath, err)
	}
	defer f.Close()

	fmt.Printf("Mode: raw image (%s)\n", imagePath)

	safeDev, safeErr := vfs.NewSafeDevice(f, imagePath)
	if safeErr != nil {
		return fmt.Errorf("failed to initialize safe device: %w", safeErr)
	}
	defer safeDev.Close()

	filesys, err := openFilesystem(safeDev)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	fmt.Printf("Detected filesystem: %s\n", filesys.Type())
	return mountFilesystem(filesys, mountPoint, filesys.Type())
}

func runFsck(devicePath string, fix bool) error {
	if fix {
		f, err := os.OpenFile(devicePath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("cannot open %q for writing (need --fix): %w", devicePath, err)
		}
		defer f.Close()
		return runFsckDevice(f, fix)
	}
	f, err := os.Open(devicePath)
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", devicePath, err)
	}
	defer f.Close()
	return runFsckDevice(f, fix)
}

func runFsckPartition(devicePath string, partNum int, fix bool) error {
	f, err := os.OpenFile(devicePath, os.O_RDWR, 0)
	if err != nil {
		f, err = os.Open(devicePath)
		if err != nil {
			return fmt.Errorf("failed to open %q: %w", devicePath, err)
		}
	}
	defer f.Close()

	_, partitions, err := block.ProbePartitions(f)
	if err != nil {
		return fmt.Errorf("failed to read partition table: %w", err)
	}

	var target *block.Partition
	for i := range partitions {
		if partitions[i].Number == partNum {
			target = &partitions[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("partition %d not found", partNum)
	}

	fmt.Printf("Checking partition %d (%s): offset %d, size %d MiB\n",
		target.Number, target.Name, target.StartOffset(), target.ByteSize()/(1024*1024))

	pr := block.NewPartitionReader(f, target)
	return runFsckDevice(pr, fix)
}

func runFsckDevice(dev block.ReadWriterAt, fix bool) error {
	filesys, err := openFilesystem(dev)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem for fsck: %w", err)
	}

	if filesys.Type() == "ext4" {
		fs, err := vfs.NewFileSystem(dev)
		if err != nil {
			return fmt.Errorf("failed to init filesystem: %w", err)
		}

		if _, err := fs.ReadSuperBlock(); err != nil {
			return fmt.Errorf("failed to read superblock: %w", err)
		}

		sb, err := fs.Superblock()
		if err != nil {
			return fmt.Errorf("superblock not available: %w", err)
		}
		fmt.Printf("Detected filesystem: ext4\n")
		fmt.Printf("Volume: %s\n", strings.Trim(string(sb.S_volume_name[:]), "\x00"))
		fmt.Printf("Block size: %d\n", fs.BlockSize)
		fmt.Printf("Inodes: %d\n", sb.S_inodes_count)
		fmt.Printf("Blocks: %d\n", sb.S_blocks_count_lo)
		fmt.Printf("Block groups: %d\n", fs.GroupCount)
		fmt.Println()

		if err := fs.ReadGroupDescriptors(); err != nil {
			return fmt.Errorf("failed to read group descriptors: %w", err)
		}

		if fix {
			fmt.Println("Repair mode enabled (--fix)")
			fmt.Println()
		}

		fmt.Println("Running integrity check...")
		res := fs.Fsck(fix)
		fmt.Println()
		fmt.Print(res.String())

		if !res.Healthy {
			return fmt.Errorf("filesystem has errors")
		}
		return nil
	} else if filesys.Type() == "btrfs" {
		btrfsfs, ok := filesys.(*btrfs.FileSystem)
		if !ok {
			return fmt.Errorf("internal error: expected btrfs filesystem")
		}

		fmt.Printf("Detected filesystem: btrfs\n")
		fmt.Printf("Node size:    %d\n", btrfsfs.BlockSize())
		fmt.Println()

		if fix {
			fmt.Println("Repair mode enabled (--fix)")
			fmt.Println("Note: Btrfs auto-repair is currently not supported.")
			fmt.Println()
		}

		fmt.Println("Running integrity check...")
		res := btrfsfs.Fsck()
		fmt.Println()
		fmt.Print(res.String())

		if !res.Healthy {
			return fmt.Errorf("filesystem has errors")
		}
		return nil
	}

	return fmt.Errorf("unsupported filesystem for fsck: %s", filesys.Type())
}

func runImageMode() error {
	file, err := os.OpenFile("testfs.img", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("failed to open testfs.img: %w", err)
	}
	defer file.Close()

	fmt.Println("Mode: raw image (testfs.img)")

	filesys, err := openFilesystem(file)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	return readRootDir(filesys)
}

func readRootDir(filesys fs.FileSystem) error {
	entries, err := filesys.Readdir("/")
	if err != nil {
		return fmt.Errorf("failed to read root directory: %w", err)
	}

	fmt.Printf("Filesystem type: %s\n", filesys.Type())
	fmt.Printf("\nRoot directory listing (%d entries):\n", len(entries))
	fmt.Printf("  %-6s  %-8s  %s\n", "type", "inode", "name")
	fmt.Printf("  %-6s  %-8s  %s\n", "------", "--------", "----")
	for _, e := range entries {
		typeName := fs.DirFileTypeName[e.FileType]
		fmt.Printf("  %-6s  %-8d  %s\n", typeName, e.Inode, e.Name)
	}
	return nil
}

func runImagePath(path string, f *os.File) error {
	fmt.Printf("Mode: raw image (%s)\n", path)
	filesys, err := openFilesystem(f)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}
	return readRootDir(filesys)
}

func runListPartitions(devicePath string) error {
	f, err := os.Open(devicePath)
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", devicePath, err)
	}
	defer f.Close()

	scheme, partitions, err := block.ProbePartitions(f)
	if err != nil {
		f.Seek(0, 0)
		return runImagePath(devicePath, f)
	}

	fmt.Printf("Device: %s\n", devicePath)
	fmt.Printf("Partition table: %s\n\n", scheme)
	fmt.Printf("  %-4s  %-24s  %-18s  %s\n", "#", "Name", "Type", "Size")
	fmt.Printf("  %-4s  %-24s  %-18s  %s\n", "----", "------------------------", "------------------", "--------")

	for _, p := range partitions {
		sizeMiB := p.ByteSize() / (1024 * 1024)
		fmt.Printf("  %-4d  %-24s  %-18s  %d MiB\n",
			p.Number, truncate(p.Name, 24), p.Type, sizeMiB,
		)
	}

	fmt.Printf("\nTo read a partition: werunos %s <partition-number>\n", devicePath)
	return nil
}

func runReadPartition(devicePath string, partNum int) error {
	f, err := os.Open(devicePath)
	if err != nil {
		return fmt.Errorf("failed to open %q: %w", devicePath, err)
	}
	defer f.Close()

	_, partitions, err := block.ProbePartitions(f)
	if err != nil {
		return fmt.Errorf("failed to probe partition table: %w", err)
	}

	var target *block.Partition
	for i := range partitions {
		if partitions[i].Number == partNum {
			target = &partitions[i]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("partition %d not found on %q", partNum, devicePath)
	}

	if target.Type != block.TypeLinuxData {
		return fmt.Errorf(
			"partition %d is %q, not a Linux filesystem",
			partNum, target.Type,
		)
	}

	fmt.Printf("Opening partition %d (%s) from %s\n", target.Number, target.Name, devicePath)
	fmt.Printf("  Offset: %d bytes, Size: %d MiB\n\n", target.StartOffset(), target.ByteSize()/(1024*1024))

	pr := block.NewPartitionReader(f, target)
	filesys, err := openFilesystem(pr)
	if err != nil {
		return fmt.Errorf("failed to detect filesystem: %w", err)
	}

	return readRootDir(filesys)
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func sanitizeVolName(s string) string {
	runes := []rune(s)
	for i, r := range runes {
		switch r {
		case '<', '>', '"', ',', ' ', '\t', '\n', '\r':
			runes[i] = '_'
		}
	}
	result := string(runes)
	if result == "" {
		return "ext4"
	}
	return result
}
