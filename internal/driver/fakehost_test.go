package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type fakeMount struct {
	source         string
	filesystemType string
	options        []string
}

// fakeHost records mounts in memory and creates the target directories and files on the real file system, where
// csi-sanity checks for them.
type fakeHost struct {
	mutex          sync.Mutex
	mounts         map[string]fakeMount
	formatted      map[string]string
	missingSerials map[string]bool
	resized        []string
	blockDevices   map[string]bool
	mountError     error
}

var _ Host = (*fakeHost)(nil)

func newFakeHost() *fakeHost {
	return &fakeHost{mounts: map[string]fakeMount{}, formatted: map[string]string{}, missingSerials: map[string]bool{},
		blockDevices: map[string]bool{}}
}

func (host *fakeHost) FindDevice(_ context.Context, serial string, _ time.Duration) (string, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if host.missingSerials[serial] {
		return "", fmt.Errorf("%w: virtio-%s", ErrDeviceNotFound, serial)
	}
	return "/dev/fake-" + serial, nil
}

func (host *fakeHost) FormatAndMount(source string, target string, filesystemType string, options []string) error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if host.mountError != nil {
		return host.mountError
	}
	if _, isFormatted := host.formatted[source]; !isFormatted {
		host.formatted[source] = filesystemType
	}
	host.mounts[target] = fakeMount{source: source, filesystemType: host.formatted[source], options: options}
	return nil
}

func (host *fakeHost) Mount(source string, target string, filesystemType string, options []string) error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if host.mountError != nil {
		return host.mountError
	}
	host.mounts[target] = fakeMount{source: source, filesystemType: filesystemType, options: options}
	if _, isDevice := host.formatted[source]; !isDevice && filepath.Dir(source) == "/dev" {
		host.blockDevices[target] = true
	}
	return nil
}

func (host *fakeHost) IsMountPoint(path string) (bool, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	_, isMounted := host.mounts[path]
	return isMounted, nil
}

func (host *fakeHost) CleanupMountPoint(path string) error {
	host.mutex.Lock()
	delete(host.mounts, path)
	delete(host.blockDevices, path)
	host.mutex.Unlock()
	if removeError := os.Remove(path); removeError != nil && !errors.Is(removeError, os.ErrNotExist) {
		return removeError
	}
	return nil
}

func (host *fakeHost) Unmount(path string) error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	if _, isMounted := host.mounts[path]; !isMounted {
		return fmt.Errorf("%s is not mounted", path)
	}
	delete(host.mounts, path)
	return nil
}

func (host *fakeHost) PathExists(path string) (bool, error) {
	host.mutex.Lock()
	_, isMounted := host.mounts[path]
	host.mutex.Unlock()
	if isMounted {
		return true, nil
	}
	_, statError := os.Stat(path)
	if errors.Is(statError, os.ErrNotExist) {
		return false, nil
	}
	return statError == nil, statError
}

func (host *fakeHost) MakeDirectory(path string) error { return os.MkdirAll(path, 0o750) }

func (host *fakeHost) MakeFile(path string) error {
	if mkdirError := os.MkdirAll(filepath.Dir(path), 0o750); mkdirError != nil {
		return mkdirError
	}
	return os.WriteFile(path, nil, 0o640)
}

func (host *fakeHost) IsBlockDevice(path string) (bool, error) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	return host.blockDevices[path], nil
}

func (host *fakeHost) ResizeFilesystem(device string, mountPath string) error {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	host.resized = append(host.resized, device+"@"+mountPath)
	return nil
}

func (host *fakeHost) FilesystemStats(string) (FilesystemStats, error) {
	return FilesystemStats{TotalBytes: 10 << 30, AvailableBytes: 9 << 30, UsedBytes: 1 << 30, TotalInodes: 655360, FreeInodes: 655000, UsedInodes: 360}, nil
}

func (host *fakeHost) BlockDeviceSizeBytes(string) (int64, error) { return 10 << 30, nil }

func (host *fakeHost) mountAt(path string) (fakeMount, bool) {
	host.mutex.Lock()
	defer host.mutex.Unlock()
	mounted, isMounted := host.mounts[path]
	return mounted, isMounted
}
