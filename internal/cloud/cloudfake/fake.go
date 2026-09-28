// Package cloudfake is an in-memory cloud.API for tests: operations finish at once, and every call can be made to
// fail with an API status.
package cloudfake

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// MaximumAttachedStorages mirrors the API's limit of storages on one server (domain.MaximumStorageDevices).
const MaximumAttachedStorages = 16

// API is the fake. The zero value is not usable; call New.
type API struct {
	mutex      sync.Mutex
	volumes    map[string]*cloud.Volume
	snapshots  map[string]*cloud.Snapshot
	servers    map[string]cloud.Server
	operations map[string]cloud.Operation
	sequence   int
	// Failures maps a method name ("CreateVolume", "AttachVolume", …) to the API status it answers with.
	failures map[string]int
	calls    map[string]int
	now      func() time.Time
}

var _ cloud.API = (*API)(nil)

// New returns an empty fake.
func New() *API {
	return &API{volumes: map[string]*cloud.Volume{}, snapshots: map[string]*cloud.Snapshot{}, servers: map[string]cloud.Server{},
		operations: map[string]cloud.Operation{}, failures: map[string]int{}, calls: map[string]int{},
		now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }}
}

// AddServer registers a server.
func (fake *API) AddServer(server cloud.Server) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.servers[server.ID] = server
}

// AddVolume registers a storage as is.
func (fake *API) AddVolume(volume cloud.Volume) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if volume.DeviceSerial == "" {
		volume.DeviceSerial = cloud.DeviceSerialFor(volume.ID)
	}
	copied := volume
	fake.volumes[volume.ID] = &copied
}

// AddSnapshot registers a snapshot as is.
func (fake *API) AddSnapshot(snapshot cloud.Snapshot) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	copied := snapshot
	fake.snapshots[snapshot.ID] = &copied
}

// FailWith makes every later call of method answer with statusCode; 0 clears it.
func (fake *API) FailWith(method string, statusCode int) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if statusCode == 0 {
		delete(fake.failures, method)
		return
	}
	fake.failures[method] = statusCode
}

// Calls is how often method was called.
func (fake *API) Calls(method string) int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.calls[method]
}

// Volume returns a copy of a storage.
func (fake *API) Volume(volumeID string) (cloud.Volume, bool) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Volume{}, false
	}
	return *volume, true
}

// VolumeCount is how many storages exist.
func (fake *API) VolumeCount() int {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return len(fake.volumes)
}

func (fake *API) enter(method string) error {
	fake.calls[method]++
	if statusCode, isFailing := fake.failures[method]; isFailing {
		return cloud.NewError(statusCode, "injected failure")
	}
	return nil
}

func (fake *API) nextIdentifier(prefix string) string {
	fake.sequence++
	return fmt.Sprintf("%s%08d-0000-4000-8000-%012d", prefix, fake.sequence, fake.sequence)
}

func (fake *API) succeeded() cloud.Operation {
	operation := cloud.Operation{ID: fake.nextIdentifier("0e"), Status: cloud.OperationStatusSucceeded}
	fake.operations[operation.ID] = operation
	return operation
}

func notFound(kind string, identifier string) error {
	return cloud.NewError(http.StatusNotFound, fmt.Sprintf("%s %s not found", kind, identifier))
}

// ListVolumes returns every storage, newest first.
func (fake *API) ListVolumes(context.Context) ([]cloud.Volume, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("ListVolumes"); failure != nil {
		return nil, failure
	}
	volumes := make([]cloud.Volume, 0, len(fake.volumes))
	for _, volume := range fake.volumes {
		volumes = append(volumes, *volume)
	}
	sort.Slice(volumes, func(first, second int) bool { return volumes[first].ID > volumes[second].ID })
	return volumes, nil
}

// GetVolume reads one storage.
func (fake *API) GetVolume(_ context.Context, volumeID string) (cloud.Volume, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("GetVolume"); failure != nil {
		return cloud.Volume{}, failure
	}
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Volume{}, notFound("storage", volumeID)
	}
	return *volume, nil
}

// CreateVolume creates a storage, online at once.
func (fake *API) CreateVolume(_ context.Context, request cloud.CreateVolumeRequest) (cloud.Volume, cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("CreateVolume"); failure != nil {
		return cloud.Volume{}, cloud.Operation{}, failure
	}
	if request.Zone == "" || request.Title == "" || request.Tier == "" {
		return cloud.Volume{}, cloud.Operation{}, cloud.NewError(http.StatusBadRequest, "zone, title and tier are required")
	}
	size := request.SizeGibibytes
	switch {
	case request.SourceSnapshotID != "":
		snapshot, isPresent := fake.snapshots[request.SourceSnapshotID]
		if !isPresent {
			return cloud.Volume{}, cloud.Operation{}, notFound("snapshot", request.SourceSnapshotID)
		}
		size = max(size, snapshot.SizeGibibytes)
	case request.SourceStorageID != "":
		source, isPresent := fake.volumes[request.SourceStorageID]
		if !isPresent {
			return cloud.Volume{}, cloud.Operation{}, notFound("storage", request.SourceStorageID)
		}
		size = max(size, source.SizeGibibytes)
	}
	if size <= 0 {
		return cloud.Volume{}, cloud.Operation{}, cloud.NewError(http.StatusBadRequest, "size_gibibytes is required")
	}
	identifier := fake.nextIdentifier("5a")
	volume := &cloud.Volume{ID: identifier, Zone: request.Zone, Title: request.Title, Tier: request.Tier,
		State: cloud.StorageStateOnline, SizeGibibytes: size, DeviceSerial: cloud.DeviceSerialFor(identifier), CreatedAt: fake.now()}
	fake.volumes[identifier] = volume
	return *volume, fake.succeeded(), nil
}

// DeleteVolume deletes a detached storage.
func (fake *API) DeleteVolume(_ context.Context, volumeID string) (cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("DeleteVolume"); failure != nil {
		return cloud.Operation{}, failure
	}
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Operation{}, notFound("storage", volumeID)
	}
	if volume.ServerID != "" {
		return cloud.Operation{}, cloud.NewError(http.StatusConflict, "the storage is attached")
	}
	delete(fake.volumes, volumeID)
	return fake.succeeded(), nil
}

// AttachVolume attaches a storage to a server.
func (fake *API) AttachVolume(_ context.Context, volumeID string, serverID string) (cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("AttachVolume"); failure != nil {
		return cloud.Operation{}, failure
	}
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Operation{}, notFound("storage", volumeID)
	}
	server, serverIsPresent := fake.servers[serverID]
	if !serverIsPresent {
		return cloud.Operation{}, notFound("server", serverID)
	}
	if volume.ServerID != "" {
		return cloud.Operation{}, cloud.NewError(http.StatusConflict, "the storage is attached to another server")
	}
	if volume.Zone != server.Zone {
		return cloud.Operation{}, cloud.NewError(http.StatusConflict, "the storage and the server are in different zones")
	}
	attached := 1
	for _, other := range fake.volumes {
		if other.ServerID == serverID {
			attached++
		}
	}
	if attached >= MaximumAttachedStorages {
		return cloud.Operation{}, cloud.NewError(http.StatusUnprocessableEntity, "the server has no free storage slot")
	}
	volume.ServerID = serverID
	return fake.succeeded(), nil
}

// DetachVolume detaches a storage.
func (fake *API) DetachVolume(_ context.Context, volumeID string) (cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("DetachVolume"); failure != nil {
		return cloud.Operation{}, failure
	}
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Operation{}, notFound("storage", volumeID)
	}
	if volume.ServerID == "" {
		return cloud.Operation{}, cloud.NewError(http.StatusConflict, "the storage is not attached")
	}
	volume.ServerID = ""
	return fake.succeeded(), nil
}

// ResizeVolume grows a storage.
func (fake *API) ResizeVolume(_ context.Context, volumeID string, sizeGibibytes int64) (cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("ResizeVolume"); failure != nil {
		return cloud.Operation{}, failure
	}
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Operation{}, notFound("storage", volumeID)
	}
	if sizeGibibytes < volume.SizeGibibytes {
		return cloud.Operation{}, cloud.NewError(http.StatusBadRequest, "storages only grow")
	}
	volume.SizeGibibytes = sizeGibibytes
	return fake.succeeded(), nil
}

// CreateSnapshot snapshots a storage, online at once.
func (fake *API) CreateSnapshot(_ context.Context, volumeID string, title string) (cloud.Snapshot, cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("CreateSnapshot"); failure != nil {
		return cloud.Snapshot{}, cloud.Operation{}, failure
	}
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Snapshot{}, cloud.Operation{}, notFound("storage", volumeID)
	}
	snapshot := &cloud.Snapshot{ID: fake.nextIdentifier("5b"), StorageID: volumeID, Zone: volume.Zone, Title: title,
		State: cloud.StorageStateOnline, SizeGibibytes: volume.SizeGibibytes, CreatedAt: fake.now()}
	fake.snapshots[snapshot.ID] = snapshot
	return *snapshot, fake.succeeded(), nil
}

// ListVolumeSnapshots lists the snapshots of one storage.
func (fake *API) ListVolumeSnapshots(_ context.Context, volumeID string) ([]cloud.Snapshot, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("ListVolumeSnapshots"); failure != nil {
		return nil, failure
	}
	if _, isPresent := fake.volumes[volumeID]; !isPresent {
		return nil, notFound("storage", volumeID)
	}
	var snapshots []cloud.Snapshot
	for _, snapshot := range fake.snapshots {
		if snapshot.StorageID == volumeID {
			snapshots = append(snapshots, *snapshot)
		}
	}
	sort.Slice(snapshots, func(first, second int) bool { return snapshots[first].ID > snapshots[second].ID })
	return snapshots, nil
}

// GetSnapshot reads one snapshot.
func (fake *API) GetSnapshot(_ context.Context, snapshotID string) (cloud.Snapshot, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("GetSnapshot"); failure != nil {
		return cloud.Snapshot{}, failure
	}
	snapshot, isPresent := fake.snapshots[snapshotID]
	if !isPresent {
		return cloud.Snapshot{}, notFound("snapshot", snapshotID)
	}
	return *snapshot, nil
}

// DeleteSnapshot deletes a snapshot.
func (fake *API) DeleteSnapshot(_ context.Context, snapshotID string) (cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("DeleteSnapshot"); failure != nil {
		return cloud.Operation{}, failure
	}
	if _, isPresent := fake.snapshots[snapshotID]; !isPresent {
		return cloud.Operation{}, notFound("snapshot", snapshotID)
	}
	delete(fake.snapshots, snapshotID)
	return fake.succeeded(), nil
}

// GetServer reads a server.
func (fake *API) GetServer(_ context.Context, serverID string) (cloud.Server, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("GetServer"); failure != nil {
		return cloud.Server{}, failure
	}
	server, isPresent := fake.servers[serverID]
	if !isPresent {
		return cloud.Server{}, notFound("server", serverID)
	}
	return server, nil
}

// GetOperation reads an operation.
func (fake *API) GetOperation(_ context.Context, operationID string) (cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("GetOperation"); failure != nil {
		return cloud.Operation{}, failure
	}
	operation, isPresent := fake.operations[operationID]
	if !isPresent {
		return cloud.Operation{}, notFound("operation", operationID)
	}
	return operation, nil
}

// SetOperation stores an operation, so a test can hand out one that is still running.
func (fake *API) SetOperation(operation cloud.Operation) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.operations[operation.ID] = operation
}
