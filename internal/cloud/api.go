// Package cloud is the narrow slice of the Ankra Cloud public API the CSI driver needs: storages (volumes), their
// snapshots, servers and operations. The driver depends only on the API interface; Client implements it with the
// generated client in internal/ankraapi and, for the operations that client does not have yet, with thin HTTP
// calls (pending.go). Tests use cloudfake.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ankraio/ankra-cloud-csi/internal/ankraapi"
)

// DeviceSerialLength is how many characters of a storage id make up the virtio serial of its disk in the guest
// (`/dev/disk/by-id/virtio-<serial>`).
const DeviceSerialLength = 20

// Storage states the driver reacts to.
const (
	StorageStateCreating    = "creating"
	StorageStateOnline      = "online"
	StorageStateMaintenance = "maintenance"
	StorageStateError       = "error"
	StorageStateDeleting    = "deleting"
	StorageStateDeleted     = "deleted"
)

// Operation statuses.
const (
	OperationStatusPending   = "pending"
	OperationStatusRunning   = "running"
	OperationStatusSucceeded = "succeeded"
	OperationStatusFailed    = "failed"
	OperationStatusCancelled = "cancelled"
)

// Volume is an Ankra Cloud storage as the driver sees it.
type Volume struct {
	ID            string
	Zone          string
	Title         string
	Tier          string
	State         string
	SizeGibibytes int64
	// ServerID is the server the storage is attached to, empty when it is detached.
	ServerID string
	// DeviceSerial is the virtio serial the guest sees; DeviceSerialFor(ID) when the API does not report one.
	DeviceSerial string
	// ActiveOperationID is the operation currently working on the storage, if any.
	ActiveOperationID string
	CreatedAt         time.Time
}

// Snapshot is a point-in-time copy of a storage.
type Snapshot struct {
	ID            string
	StorageID     string
	Zone          string
	Title         string
	State         string
	SizeGibibytes int64
	CreatedAt     time.Time
}

// IsReady reports whether a volume can be created from the snapshot.
func (snapshot Snapshot) IsReady() bool {
	return snapshot.State == StorageStateOnline || snapshot.State == "available" || snapshot.State == "ready"
}

// Server is the part of a server the driver reads: its identity, zone and power state.
type Server struct {
	ID    string
	Zone  string
	State string
}

// Operation is an asynchronous change the API is carrying out.
type Operation struct {
	ID     string
	Status string
	Step   string
	Error  string
}

// IsFinished reports whether the operation reached a final status.
func (operation Operation) IsFinished() bool {
	switch operation.Status {
	case OperationStatusSucceeded, OperationStatusFailed, OperationStatusCancelled:
		return true
	}
	return false
}

// CreateVolumeRequest creates an empty storage, a clone of a storage, or a storage from a snapshot. At most one
// source is set.
type CreateVolumeRequest struct {
	Zone             string
	Title            string
	Tier             string
	SizeGibibytes    int64
	SourceStorageID  string
	SourceSnapshotID string
	// Labels are sent once storages carry labels; until then the title is the driver's idempotency key.
	Labels map[string]string
}

// API is every call the driver makes. Errors from the API are *ankraapi.Error, so StatusCode tells them apart.
type API interface {
	ListVolumes(ctx context.Context) ([]Volume, error)
	GetVolume(ctx context.Context, volumeID string) (Volume, error)
	CreateVolume(ctx context.Context, request CreateVolumeRequest) (Volume, Operation, error)
	DeleteVolume(ctx context.Context, volumeID string) (Operation, error)
	// AttachVolume hot-plugs the storage into a running (or stopped) server.
	AttachVolume(ctx context.Context, volumeID string, serverID string) (Operation, error)
	// DetachVolume hot-unplugs the storage from its server.
	DetachVolume(ctx context.Context, volumeID string) (Operation, error)
	// ResizeVolume grows the storage, online when it is attached to a running server.
	ResizeVolume(ctx context.Context, volumeID string, sizeGibibytes int64) (Operation, error)

	CreateSnapshot(ctx context.Context, volumeID string, title string) (Snapshot, Operation, error)
	ListVolumeSnapshots(ctx context.Context, volumeID string) ([]Snapshot, error)
	GetSnapshot(ctx context.Context, snapshotID string) (Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapshotID string) (Operation, error)

	GetServer(ctx context.Context, serverID string) (Server, error)
	GetOperation(ctx context.Context, operationID string) (Operation, error)
}

// DeviceSerialFor is the virtio serial of a storage's disk: the first DeviceSerialLength characters of its id.
func DeviceSerialFor(storageID string) string {
	if len(storageID) <= DeviceSerialLength {
		return storageID
	}
	return storageID[:DeviceSerialLength]
}

// StatusCode is the HTTP status of an API error, 0 for any other error.
func StatusCode(callError error) int {
	var apiError *ankraapi.Error
	if errors.As(callError, &apiError) {
		return apiError.StatusCode
	}
	return 0
}

// IsNotFound reports whether the API answered 404.
func IsNotFound(callError error) bool {
	return StatusCode(callError) == http.StatusNotFound
}

// NewError builds the error the API would return, for fakes and the thin HTTP calls.
func NewError(statusCode int, detail string) error {
	return &ankraapi.Error{StatusCode: statusCode, Title: http.StatusText(statusCode), Detail: detail}
}

// ErrOperationFailed is returned by WaitForOperation when the operation ends in failure or is cancelled.
var ErrOperationFailed = errors.New("operation did not succeed")

// WaitForOperation polls the operation until it finishes or ctx ends.
func WaitForOperation(ctx context.Context, api API, operation Operation, interval time.Duration) (Operation, error) {
	current := operation
	if current.ID == "" {
		return current, nil
	}
	for {
		if current.IsFinished() {
			if current.Status != OperationStatusSucceeded {
				return current, fmt.Errorf("%w: operation %s %s at step %q: %s", ErrOperationFailed, current.ID, current.Status, current.Step, current.Error)
			}
			return current, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return current, ctx.Err()
		case <-timer.C:
		}
		refreshed, getError := api.GetOperation(ctx, current.ID)
		if getError != nil {
			return current, fmt.Errorf("get operation %s: %w", current.ID, getError)
		}
		current = refreshed
	}
}
