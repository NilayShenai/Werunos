//go:build darwin

package block

import (
	"fmt"
	"os"
)

const maxDisks = 32

func EnumerateDisks() ([]string, error) {
	var disks []string

	for i := 0; i < maxDisks; i++ {
		// Use /dev/rdiskN on macOS for high-performance raw disk access
		path := fmt.Sprintf("/dev/rdisk%d", i)

		f, err := os.Open(path)
		if err != nil {
			// If we get Permission Denied, it means the disk exists but requires root privileges to read.
			if os.IsPermission(err) {
				disks = append(disks, path)
				continue
			}
			// If we hit a "no such file or directory" error, we have reached the end of valid physical disk numbers.
			break
		}
		f.Close()

		disks = append(disks, path)
	}

	return disks, nil
}
