//go:build windows

package block

import (
	"fmt"
	"os"
)

const maxDisks = 16

func EnumerateDisks() ([]string, error) {
	var disks []string

	for i := range maxDisks {

		path := fmt.Sprintf(`\\.\PhysicalDrive%d`, i)

		f, err := os.Open(path)
		if err != nil {

			break
		}
		f.Close()

		disks = append(disks, path)
	}

	return disks, nil
}
