package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mount "k8s.io/mount-utils"
	utilexec "k8s.io/utils/exec"
)

// DeviceByIDDirectory is where udev links virtio disks by serial.
const DeviceByIDDirectory = "/dev/disk/by-id"

// ErrDeviceNotFound is returned when the hot-plugged disk does not show up in the guest.
var ErrDeviceNotFound = errors.New("device not found")

// FilesystemStats is what NodeGetVolumeStats reports for a mounted volume.
type FilesystemStats struct {
	TotalBytes     int64
	AvailableBytes int64
	UsedBytes      int64
	TotalInodes    int64
	FreeInodes     int64
	UsedInodes     int64
}

// Host is everything the node service does to the machine: find disks, format, mount, grow and measure. HostOS
// implements it with k8s.io/mount-utils; tests use a fake.
type Host interface {
	// FindDevice waits up to timeout for /dev/disk/by-id/virtio-<serial> and returns the device it links to.
	FindDevice(ctx context.Context, serial string, timeout time.Duration) (string, error)
	FormatAndMount(source string, target string, filesystemType string, options []string) error
	Mount(source string, target string, filesystemType string, options []string) error
	IsMountPoint(path string) (bool, error)
	// CleanupMountPoint unmounts the path if it is mounted and removes it.
	CleanupMountPoint(path string) error
	Unmount(path string) error
	PathExists(path string) (bool, error)
	MakeDirectory(path string) error
	MakeFile(path string) error
	IsBlockDevice(path string) (bool, error)
	// ResizeFilesystem grows the file system on device, mounted at mountPath, to the device's size.
	ResizeFilesystem(device string, mountPath string) error
	FilesystemStats(path string) (FilesystemStats, error)
	BlockDeviceSizeBytes(path string) (int64, error)
}

// HostOS is the real Host.
type HostOS struct {
	mounter *mount.SafeFormatAndMount
	exec    utilexec.Interface
}

var _ Host = (*HostOS)(nil)

// NewHostOS builds the real Host.
func NewHostOS() *HostOS {
	executor := utilexec.New()
	return &HostOS{mounter: mount.NewSafeFormatAndMount(mount.New(""), executor), exec: executor}
}

// FindDevice waits for the udev link of the virtio serial.
func (host *HostOS) FindDevice(ctx context.Context, serial string, timeout time.Duration) (string, error) {
	link := filepath.Join(DeviceByIDDirectory, "virtio-"+serial)
	deadline := time.Now().Add(timeout)
	for {
		resolved, resolveError := filepath.EvalSymlinks(link)
		if resolveError == nil {
			return resolved, nil
		}
		if !errors.Is(resolveError, os.ErrNotExist) {
			return "", fmt.Errorf("resolve %s: %w", link, resolveError)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%w: %s did not appear within %s", ErrDeviceNotFound, link, timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// FormatAndMount formats the device when it holds no file system, then mounts it.
func (host *HostOS) FormatAndMount(source string, target string, filesystemType string, options []string) error {
	return host.mounter.FormatAndMount(source, target, filesystemType, options)
}

// Mount mounts source at target.
func (host *HostOS) Mount(source string, target string, filesystemType string, options []string) error {
	return host.mounter.Mount(source, target, filesystemType, options)
}

// IsMountPoint reports whether path is a mount point; a missing path is not.
func (host *HostOS) IsMountPoint(path string) (bool, error) {
	isMountPoint, checkError := host.mounter.IsMountPoint(path)
	if errors.Is(checkError, os.ErrNotExist) {
		return false, nil
	}
	return isMountPoint, checkError
}

// CleanupMountPoint unmounts and removes path.
func (host *HostOS) CleanupMountPoint(path string) error {
	return mount.CleanupMountPoint(path, host.mounter, true)
}

// Unmount unmounts path.
func (host *HostOS) Unmount(path string) error {
	return host.mounter.Unmount(path)
}

// PathExists reports whether path exists.
func (host *HostOS) PathExists(path string) (bool, error) {
	return mount.PathExists(path)
}

// MakeDirectory creates path and its parents.
func (host *HostOS) MakeDirectory(path string) error {
	return os.MkdirAll(path, 0o750)
}

// MakeFile creates an empty file at path, the bind target of a block volume.
func (host *HostOS) MakeFile(path string) error {
	if mkdirError := os.MkdirAll(filepath.Dir(path), 0o750); mkdirError != nil {
		return mkdirError
	}
	file, openError := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if openError != nil {
		return openError
	}
	return file.Close()
}

// IsBlockDevice reports whether path is a block device.
func (host *HostOS) IsBlockDevice(path string) (bool, error) {
	info, statError := os.Stat(path)
	if statError != nil {
		return false, statError
	}
	return info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0, nil
}

// ResizeFilesystem runs resize2fs or xfs_growfs, whichever the file system needs.
func (host *HostOS) ResizeFilesystem(device string, mountPath string) error {
	_, resizeError := mount.NewResizeFs(host.exec).Resize(device, mountPath)
	return resizeError
}

// FilesystemStats reads statfs of path.
func (host *HostOS) FilesystemStats(path string) (FilesystemStats, error) {
	return filesystemStats(path)
}

// BlockDeviceSizeBytes asks blockdev for the size of a block device.
func (host *HostOS) BlockDeviceSizeBytes(path string) (int64, error) {
	output, runError := host.exec.Command("blockdev", "--getsize64", path).CombinedOutput()
	if runError != nil {
		return 0, fmt.Errorf("blockdev --getsize64 %s: %w: %s", path, runError, strings.TrimSpace(string(output)))
	}
	size, parseError := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if parseError != nil {
		return 0, fmt.Errorf("parse the size of %s: %w", path, parseError)
	}
	return size, nil
}
