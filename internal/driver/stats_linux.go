//go:build linux

package driver

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func filesystemStats(path string) (FilesystemStats, error) {
	var statistics unix.Statfs_t
	if statError := unix.Statfs(path, &statistics); statError != nil {
		return FilesystemStats{}, fmt.Errorf("statfs %s: %w", path, statError)
	}
	blockSize := statistics.Bsize
	total := int64(statistics.Blocks) * blockSize
	available := int64(statistics.Bavail) * blockSize
	free := int64(statistics.Bfree) * blockSize
	return FilesystemStats{
		TotalBytes:     total,
		AvailableBytes: available,
		UsedBytes:      total - free,
		TotalInodes:    int64(statistics.Files),
		FreeInodes:     int64(statistics.Ffree),
		UsedInodes:     int64(statistics.Files) - int64(statistics.Ffree),
	}, nil
}
