//go:build !linux

package driver

import "errors"

func filesystemStats(string) (FilesystemStats, error) {
	return FilesystemStats{}, errors.New("file system statistics are only read on Linux")
}
