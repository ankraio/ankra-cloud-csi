// Package cloudfake is an in-memory cloud.API for tests: operations finish at once unless a test makes creates or
// deletes take time (SettleCreates, SettleDeletes), and every call can be made to fail with an API status or with an
// error that has none.
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

// DefaultStorageTier is a zone's default storage tier unless SetZoneDefaultTier says otherwise.
const DefaultStorageTier = "standard"

// LocalStorageTier is the tier whose storages live on one compute node and attach only to servers on it.
const LocalStorageTier = "local-nvme"

// MaximumAttachedStorages mirrors the API's limit of storages on one server (domain.MaximumStorageDevices).
const MaximumAttachedStorages = 16

// Never, as the number of reads a storage settles after, keeps the storage as it is until SettleVolume says otherwise.
const Never = -1

// settlement is what becomes of a storage an operation is working on: the state it ends in, after that many more reads
// of the storage or of the operation.
type settlement struct {
	operationID string
	polls       int
	state       string
}

// API is the fake. The zero value is not usable; call New.
type API struct {
	mutex      sync.Mutex
	volumes    map[string]*cloud.Volume
	snapshots  map[string]*cloud.Snapshot
	servers    map[string]cloud.Server
	operations map[string]cloud.Operation
	// zoneDefaultTiers is the tier a storage created without one gets, per zone.
	zoneDefaultTiers map[string]string
	// serverNodes is the compute node each server runs on; localVolumeNodes the node each local storage lives on.
	serverNodes      map[string]string
	localVolumeNodes map[string]string
	// localRooms is the GiB the Ankra Local pool of a compute node still takes; a node without an entry reports no
	// pool and is not gated, as the API does not gate one. tierRefusals refuses a tier in every zone.
	localRooms   map[string]int64
	tierRefusals map[string]string
	sequence     int
	// Failures maps a method name ("CreateVolume", "AttachVolume", …) to the API status it answers with.
	failures map[string]int
	// failureDetails is the problem detail a failing method answers with; "injected failure" when none was given.
	failureDetails map[string]string
	// failureReasons is the reason member a failing method's problem document carries; none when it has no entry.
	failureReasons map[string]string
	calls          map[string]int
	now            func() time.Time
	// failureErrors is the error a failing method returns as it is, in place of an API status.
	failureErrors map[string]error
	// createSettlement and deleteSettlement make creates and deletes take time; nil finishes them at once.
	createSettlement *settlement
	deleteSettlement *settlement
	// settlements are the storages an operation is still working on, by storage id.
	settlements map[string]*settlement
}

var _ cloud.API = (*API)(nil)

// New returns an empty fake.
func New() *API {
	return &API{volumes: map[string]*cloud.Volume{}, snapshots: map[string]*cloud.Snapshot{}, servers: map[string]cloud.Server{},
		operations: map[string]cloud.Operation{}, failures: map[string]int{}, failureDetails: map[string]string{}, failureReasons: map[string]string{},
		failureErrors: map[string]error{}, settlements: map[string]*settlement{},
		calls:            map[string]int{},
		zoneDefaultTiers: map[string]string{}, serverNodes: map[string]string{}, localVolumeNodes: map[string]string{},
		localRooms: map[string]int64{}, tierRefusals: map[string]string{},
		now: func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) }}
}

// AddServer registers a server.
func (fake *API) AddServer(server cloud.Server) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.servers[server.ID] = server
}

// SetZoneDefaultTier sets the tier storages created in zone without one get.
func (fake *API) SetZoneDefaultTier(zone string, tier string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.zoneDefaultTiers[zone] = tier
}

// PlaceServer records the compute node a server runs on.
func (fake *API) PlaceServer(serverID string, node string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.serverNodes[serverID] = node
}

// SetLocalRoom makes the Ankra Local pool of a compute node take gibibytes more, as the API's thin-pool rule would:
// a larger local storage there is refused with the API's 409, and creating and deleting local storages there moves
// the room.
func (fake *API) SetLocalRoom(node string, gibibytes int64) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.localRooms[node] = gibibytes
}

// LocalRoom is the GiB the Ankra Local pool of a compute node still takes; false when it is not gated.
func (fake *API) LocalRoom(node string) (int64, bool) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	room, isGated := fake.localRooms[node]
	return room, isGated
}

// RefuseTier makes the API take no new storage of tier anywhere, with refusal as the reason; "" takes it again.
func (fake *API) RefuseTier(tier string, refusal string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if refusal == "" {
		delete(fake.tierRefusals, tier)
		return
	}
	fake.tierRefusals[tier] = refusal
}

// LocalVolumeNode is the compute node a local storage lives on.
func (fake *API) LocalVolumeNode(volumeID string) string {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	return fake.localVolumeNodes[volumeID]
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

// FailWith makes every later call of method answer with statusCode; 0 clears it. A detail, a reason or an error given
// for the method before is dropped.
func (fake *API) FailWith(method string, statusCode int) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	delete(fake.failureDetails, method)
	delete(fake.failureReasons, method)
	delete(fake.failureErrors, method)
	if statusCode == 0 {
		delete(fake.failures, method)
		return
	}
	fake.failures[method] = statusCode
}

// FailWithDetail makes every later call of method answer with statusCode and the problem detail the API would send.
func (fake *API) FailWithDetail(method string, statusCode int, detail string) {
	fake.FailWith(method, statusCode)
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.failureDetails[method] = detail
}

// FailWithShortfall makes every later call of method answer as the API answers a platform shortfall: statusCode, the
// customer's wording as the detail, and reason as the reason member of the problem document.
func (fake *API) FailWithShortfall(method string, statusCode int, reason string) {
	fake.FailWithDetail(method, statusCode, cloud.StorageShortfallDetail)
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.failureReasons[method] = reason
}

// FailWithError makes every later call of method return failure as it is: an error without an HTTP status, which is
// what a call that got no answer from the API returns. FailWith(method, 0) clears it.
func (fake *API) FailWithError(method string, failure error) {
	fake.FailWith(method, 0)
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.failureErrors[method] = failure
}

// SettleCreates makes every later CreateVolume take time, as the API's does: the storage is answered `creating` behind
// a running operation, and after polls reads of the storage or of the operation it is state, cloud.StorageStateOnline
// or cloud.StorageStateError with the operation failed. Never keeps it `creating`. An empty state creates online at
// once again.
func (fake *API) SettleCreates(polls int, state string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.createSettlement = nil
	if state != "" {
		fake.createSettlement = &settlement{polls: polls, state: state}
	}
}

// SettleDeletes makes every later DeleteVolume take time: the storage is left `deleting` behind a running operation,
// and after polls reads of the storage or of the operation it is gone (cloud.StorageStateDeleted) or back in
// cloud.StorageStateError with the operation failed, which is where the API leaves a storage whose delete failed.
// Never keeps it `deleting`. An empty state deletes at once again.
func (fake *API) SettleDeletes(polls int, state string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	fake.deleteSettlement = nil
	if state != "" {
		fake.deleteSettlement = &settlement{polls: polls, state: state}
	}
}

// SettleVolume makes one storage end in state after polls more reads of it, whatever was to become of it before; 0 is
// at once.
func (fake *API) SettleVolume(volumeID string, polls int, state string) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	outcome := &settlement{polls: polls, state: state}
	if pending, isPending := fake.settlements[volumeID]; isPending {
		outcome.operationID = pending.operationID
	}
	fake.settlements[volumeID] = outcome
	if polls == 0 {
		fake.settle(volumeID, outcome)
	}
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
	if failure, isFailing := fake.failureErrors[method]; isFailing {
		return failure
	}
	if statusCode, isFailing := fake.failures[method]; isFailing {
		detail := fake.failureDetails[method]
		if detail == "" {
			detail = "injected failure"
		}
		if reason, isShortfall := fake.failureReasons[method]; isShortfall {
			return cloud.NewShortfallError(statusCode, detail, reason)
		}
		return cloud.NewError(statusCode, detail)
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

// running hands out an operation that is still working on a storage and ends as outcome says.
func (fake *API) running(volumeID string, outcome settlement) cloud.Operation {
	operation := cloud.Operation{ID: fake.nextIdentifier("0e"), Status: cloud.OperationStatusRunning}
	fake.operations[operation.ID] = operation
	outcome.operationID = operation.ID
	fake.settlements[volumeID] = &outcome
	return operation
}

// poll counts one read of a storage, or of the operation working on it, and settles the storage when its time has
// come.
func (fake *API) poll(volumeID string) {
	pending, isPending := fake.settlements[volumeID]
	if !isPending || pending.polls < 0 {
		return
	}
	if pending.polls > 0 {
		pending.polls--
		return
	}
	fake.settle(volumeID, pending)
}

// settle ends the operation working on a storage and puts the storage in the state the operation leaves it in.
func (fake *API) settle(volumeID string, outcome *settlement) {
	delete(fake.settlements, volumeID)
	operation, hasOperation := fake.operations[outcome.operationID]
	operation.Status = cloud.OperationStatusSucceeded
	volume, isPresent := fake.volumes[volumeID]
	switch {
	case !isPresent:
	case outcome.state == cloud.StorageStateDeleted:
		fake.removeVolume(volumeID)
	case outcome.state == cloud.StorageStateError:
		volume.State = cloud.StorageStateError
		operation.Status, operation.Step, operation.Error = cloud.OperationStatusFailed, "storage", "injected failure"
	default:
		volume.State = outcome.state
	}
	if hasOperation {
		fake.operations[outcome.operationID] = operation
	}
}

// removeVolume drops a storage and gives its room back to the Ankra Local pool of its node.
func (fake *API) removeVolume(volumeID string) {
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return
	}
	if node, isLocal := fake.localVolumeNodes[volumeID]; isLocal {
		if room, isGated := fake.localRooms[node]; isGated {
			fake.localRooms[node] = room + volume.SizeGibibytes
		}
	}
	delete(fake.volumes, volumeID)
	delete(fake.localVolumeNodes, volumeID)
	delete(fake.settlements, volumeID)
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
	fake.poll(volumeID)
	volume, isPresent := fake.volumes[volumeID]
	if !isPresent {
		return cloud.Volume{}, notFound("storage", volumeID)
	}
	return *volume, nil
}

// CreateVolume creates a storage, online at once unless SettleCreates makes it take time.
func (fake *API) CreateVolume(_ context.Context, request cloud.CreateVolumeRequest) (cloud.Volume, cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("CreateVolume"); failure != nil {
		return cloud.Volume{}, cloud.Operation{}, failure
	}
	if request.Zone == "" || request.Title == "" {
		return cloud.Volume{}, cloud.Operation{}, cloud.NewError(http.StatusBadRequest, "zone and title are required")
	}
	tier := request.Tier
	if tier == "" {
		tier = fake.zoneDefaultTiers[request.Zone]
	}
	if tier == "" {
		tier = DefaultStorageTier
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
	if _, isRefused := fake.tierRefusals[tier]; isRefused {
		return cloud.Volume{}, cloud.Operation{}, storageShortfall()
	}
	var node string
	if tier == LocalStorageTier {
		node = fake.localNode(request)
		if room, isGated := fake.localRooms[node]; isGated && size > room {
			return cloud.Volume{}, cloud.Operation{}, storageShortfall()
		}
	}
	identifier := fake.nextIdentifier("5a")
	if tier == LocalStorageTier {
		fake.localVolumeNodes[identifier] = node
		if room, isGated := fake.localRooms[node]; isGated {
			fake.localRooms[node] = room - size
		}
	}
	volume := &cloud.Volume{ID: identifier, Zone: request.Zone, Title: request.Title, Tier: tier,
		State: cloud.StorageStateOnline, SizeGibibytes: size, DeviceSerial: cloud.DeviceSerialFor(identifier), CreatedAt: fake.now()}
	fake.volumes[identifier] = volume
	if fake.createSettlement == nil {
		return *volume, fake.succeeded(), nil
	}
	volume.State = cloud.StorageStateCreating
	return *volume, fake.running(identifier, *fake.createSettlement), nil
}

// localNode is where a new local storage lands: next to the placement server, else on the source's node, else on
// a node no server runs on, as the API's least-loaded choice can be.
func (fake *API) localNode(request cloud.CreateVolumeRequest) string {
	if node, isPlaced := fake.serverNodes[request.PlacementServerID]; isPlaced {
		return node
	}
	if node, isPresent := fake.localVolumeNodes[request.SourceStorageID]; isPresent {
		return node
	}
	return "least-loaded-node"
}

// DeleteVolume deletes a detached storage, at once unless SettleDeletes makes it take time. A storage that is already
// being deleted is refused, as the API refuses it.
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
	if volume.State == cloud.StorageStateDeleting {
		return cloud.Operation{}, cloud.NewError(http.StatusConflict, "the storage is already being deleted")
	}
	if fake.deleteSettlement != nil {
		volume.State = cloud.StorageStateDeleting
		return fake.running(volumeID, *fake.deleteSettlement), nil
	}
	fake.removeVolume(volumeID)
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
	if storageNode, isLocal := fake.localVolumeNodes[volumeID]; isLocal && storageNode != fake.serverNodes[serverID] {
		return cloud.Operation{}, cloud.NewError(http.StatusConflict, fmt.Sprintf(
			"local-nvme volumes live on one node; the storage is on node %s and the server runs on node %s", storageNode, fake.serverNodes[serverID]))
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

// storageUnavailable opens the refusal the capacity answer carries for a tier or a node that takes no new storage.
const storageUnavailable = "the storage backend is not available in this zone"

// storageShortfall is the API's answer to a new storage whose tier, or whose node's Ankra Local pool, takes none: 409
// with the customer's wording, which says nothing about the pool, and the reason member beside it.
func storageShortfall() error {
	return cloud.NewShortfallError(http.StatusConflict, cloud.StorageShortfallDetail, cloud.ShortfallReasonNotOffered)
}

// maximumStorageGibibytes mirrors the API's domain.MaximumStorageGibibytes.
const maximumStorageGibibytes = 4096

// fakeTiers are the tiers the fake API knows.
var fakeTiers = []string{DefaultStorageTier, "hdd", LocalStorageTier}

// GetStorageCapacity answers how much new storage of each tier the fake takes next to a server, by the rules its
// CreateVolume applies.
func (fake *API) GetStorageCapacity(_ context.Context, serverID string) (cloud.StorageCapacity, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("GetStorageCapacity"); failure != nil {
		return cloud.StorageCapacity{}, failure
	}
	server, isPresent := fake.servers[serverID]
	if !isPresent {
		return cloud.StorageCapacity{}, notFound("server", serverID)
	}
	defaultTier := fake.zoneDefaultTiers[server.Zone]
	if defaultTier == "" {
		defaultTier = DefaultStorageTier
	}
	capacity := cloud.StorageCapacity{ServerID: serverID, Zone: server.Zone, DefaultTier: defaultTier}
	for _, tier := range fakeTiers {
		tierCapacity := cloud.TierCapacity{Tier: tier, MaximumGibibytes: maximumStorageGibibytes}
		if refusal, isRefused := fake.tierRefusals[tier]; isRefused {
			tierCapacity = cloud.TierCapacity{Tier: tier, Refusal: storageUnavailable + ": " + refusal}
		} else if node, isPlaced := fake.serverNodes[serverID]; tier == LocalStorageTier && isPlaced {
			if room, isGated := fake.localRooms[node]; isGated {
				available := max(room, 0)
				tierCapacity = cloud.TierCapacity{Tier: tier, MaximumGibibytes: min(available, maximumStorageGibibytes), AvailableGibibytes: &available}
				if available == 0 {
					tierCapacity.Refusal = storageUnavailable + ": the node's Ankra Local pool takes no new volumes"
				}
			}
		}
		capacity.Tiers = append(capacity.Tiers, tierCapacity)
	}
	return capacity, nil
}

// GetOperation reads an operation.
func (fake *API) GetOperation(_ context.Context, operationID string) (cloud.Operation, error) {
	fake.mutex.Lock()
	defer fake.mutex.Unlock()
	if failure := fake.enter("GetOperation"); failure != nil {
		return cloud.Operation{}, failure
	}
	for volumeID, pending := range fake.settlements {
		if pending.operationID == operationID {
			fake.poll(volumeID)
			break
		}
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
