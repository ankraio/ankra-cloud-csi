package driver

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

var controllerCapabilities = []csi.ControllerServiceCapability_RPC_Type{
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
	csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
	csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
	csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS,
	csi.ControllerServiceCapability_RPC_CLONE_VOLUME,
}

// ControllerGetCapabilities lists the controller RPCs the driver implements.
func (driver *Driver) ControllerGetCapabilities(context.Context, *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	capabilities := make([]*csi.ControllerServiceCapability, 0, len(controllerCapabilities))
	for _, capability := range controllerCapabilities {
		capabilities = append(capabilities, &csi.ControllerServiceCapability{Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{Type: capability}}})
	}
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

// operationContext bounds the wait for API operations within one RPC.
func (driver *Driver) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, driver.options.OperationTimeout)
}

func (driver *Driver) wait(ctx context.Context, operation cloud.Operation) error {
	_, waitError := cloud.WaitForOperation(ctx, driver.options.API, operation, driver.options.OperationPollInterval)
	return waitError
}

// CreateVolume finds the storage named after the volume or creates it. The volume name is the storage title: storages
// have no labels yet, so the title is the idempotency key (VolumeNameLabel is sent once they do).
func (driver *Driver) CreateVolume(ctx context.Context, request *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	name := request.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume name is required")
	}
	if len(request.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	if capabilityError := validateCapabilities(request.GetVolumeCapabilities()); capabilityError != nil {
		return nil, status.Error(codes.InvalidArgument, capabilityError.Error())
	}
	sizeGibibytes, sizeError := requestedGibibytes(request.GetCapacityRange())
	if sizeError != nil {
		return nil, sizeError
	}
	tier := request.GetParameters()[ParameterTier]
	release, isLocked := driver.lock("name/" + name)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", name)
	}
	defer release()
	ctx, cancel := driver.operationContext(ctx)
	defer cancel()

	source, sourceError := driver.resolveSource(ctx, request.GetVolumeContentSource())
	if sourceError != nil {
		return nil, sourceError
	}
	if source.sizeGibibytes > sizeGibibytes {
		if limit := request.GetCapacityRange().GetLimitBytes(); limit > 0 && source.sizeGibibytes*gibibyte > limit {
			return nil, status.Errorf(codes.OutOfRange, "the source is %d GiB, more than the limit of %d bytes", source.sizeGibibytes, limit)
		}
		sizeGibibytes = source.sizeGibibytes
	}

	existing, findError := driver.findVolumeByName(ctx, name)
	if findError != nil {
		return nil, apiStatus(findError, "look up volume %s", name)
	}
	if existing != nil && existing.State == cloud.StorageStateError {
		driver.logger.Warn("replacing a volume that failed to provision", "volume", existing.ID, "name", name)
		operation, deleteError := driver.options.API.DeleteVolume(ctx, existing.ID)
		if deleteError != nil && !cloud.IsNotFound(deleteError) {
			return nil, apiStatus(deleteError, "delete the failed volume %s", existing.ID)
		}
		if waitError := driver.wait(ctx, operation); waitError != nil {
			return nil, apiStatus(waitError, "delete the failed volume %s", existing.ID)
		}
		existing = nil
	}
	if existing != nil {
		if !fitsCapacityRange(existing.SizeGibibytes, request.GetCapacityRange()) {
			return nil, status.Errorf(codes.AlreadyExists, "volume %s exists with %d GiB, outside the requested range", name, existing.SizeGibibytes)
		}
		if tier != "" && existing.Tier != tier {
			return nil, status.Errorf(codes.AlreadyExists, "volume %s exists on tier %s, not %s", name, existing.Tier, tier)
		}
		ready, waitError := driver.waitForVolume(ctx, *existing)
		if waitError != nil {
			return nil, waitError
		}
		return &csi.CreateVolumeResponse{Volume: driver.csiVolume(ready, nodeFromTopology(request.GetAccessibilityRequirements(), ready.Zone), request)}, nil
	}

	zone, zoneError := driver.chooseZone(request.GetAccessibilityRequirements(), source.zone)
	if zoneError != nil {
		return nil, zoneError
	}
	node := nodeFromTopology(request.GetAccessibilityRequirements(), zone)
	if tier == TierLocalNVMe && node == "" {
		return nil, status.Errorf(codes.InvalidArgument,
			"tier %s needs the selected node in the accessibility requirements: use volumeBindingMode WaitForFirstConsumer", TierLocalNVMe)
	}
	created, operation, createError := driver.options.API.CreateVolume(ctx, cloud.CreateVolumeRequest{
		Zone: zone, Title: name, Tier: tier, SizeGibibytes: sizeGibibytes,
		SourceStorageID: source.storageID, SourceSnapshotID: source.snapshotID,
		PlacementServerID: placementServer(tier, node, source),
		Labels:            map[string]string{VolumeNameLabel: name},
	})
	if createError != nil {
		return nil, apiStatus(createError, "create volume %s", name)
	}
	if waitError := driver.wait(ctx, operation); waitError != nil {
		return nil, apiStatus(waitError, "create volume %s (%s)", name, created.ID)
	}
	ready, getError := driver.options.API.GetVolume(ctx, created.ID)
	if getError != nil {
		return nil, apiStatus(getError, "read volume %s", created.ID)
	}
	return &csi.CreateVolumeResponse{Volume: driver.csiVolume(ready, node, request)}, nil
}

// placementServer is the server whose compute node a new local-nvme storage lands on: the selected node, for a tier
// that is local-nvme or left to the zone's default. A clone stays on its source's node and takes no placement.
func placementServer(tier string, node string, source volumeSource) string {
	if source.storageID != "" || source.snapshotID != "" {
		return ""
	}
	if tier != TierLocalNVMe && tier != "" {
		return ""
	}
	return node
}

type volumeSource struct {
	storageID     string
	snapshotID    string
	zone          string
	sizeGibibytes int64
}

func (driver *Driver) resolveSource(ctx context.Context, contentSource *csi.VolumeContentSource) (volumeSource, error) {
	if contentSource == nil {
		return volumeSource{}, nil
	}
	if snapshotSource := contentSource.GetSnapshot(); snapshotSource != nil {
		snapshot, getError := driver.options.API.GetSnapshot(ctx, snapshotSource.GetSnapshotId())
		if getError != nil {
			return volumeSource{}, apiStatus(getError, "read source snapshot %s", snapshotSource.GetSnapshotId())
		}
		if !snapshot.IsReady() {
			return volumeSource{}, status.Errorf(codes.Unavailable, "source snapshot %s is %s, not ready", snapshot.ID, snapshot.State)
		}
		return volumeSource{snapshotID: snapshot.ID, zone: snapshot.Zone, sizeGibibytes: snapshot.SizeGibibytes}, nil
	}
	if volumeSourceReference := contentSource.GetVolume(); volumeSourceReference != nil {
		volume, getError := driver.options.API.GetVolume(ctx, volumeSourceReference.GetVolumeId())
		if getError != nil {
			return volumeSource{}, apiStatus(getError, "read source volume %s", volumeSourceReference.GetVolumeId())
		}
		return volumeSource{storageID: volume.ID, zone: volume.Zone, sizeGibibytes: volume.SizeGibibytes}, nil
	}
	return volumeSource{}, status.Error(codes.InvalidArgument, "unsupported volume content source")
}

func (driver *Driver) findVolumeByName(ctx context.Context, name string) (*cloud.Volume, error) {
	volumes, listError := driver.options.API.ListVolumes(ctx)
	if listError != nil {
		return nil, listError
	}
	for index := range volumes {
		volume := volumes[index]
		if volume.Title == name && volume.State != cloud.StorageStateDeleting && volume.State != cloud.StorageStateDeleted {
			return &volume, nil
		}
	}
	return nil, nil
}

// waitForVolume waits for a storage another call started to finish provisioning.
func (driver *Driver) waitForVolume(ctx context.Context, volume cloud.Volume) (cloud.Volume, error) {
	for volume.State != cloud.StorageStateOnline {
		if volume.State == cloud.StorageStateError {
			return volume, status.Errorf(codes.Internal, "volume %s failed to provision", volume.ID)
		}
		timer := time.NewTimer(driver.options.OperationPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return volume, status.Errorf(codes.DeadlineExceeded, "volume %s is still %s", volume.ID, volume.State)
		case <-timer.C:
		}
		refreshed, getError := driver.options.API.GetVolume(ctx, volume.ID)
		if getError != nil {
			return volume, apiStatus(getError, "read volume %s", volume.ID)
		}
		volume = refreshed
	}
	return volume, nil
}

func (driver *Driver) chooseZone(requirements *csi.TopologyRequirement, sourceZone string) (string, error) {
	var candidates []string
	for _, topology := range append(append([]*csi.Topology{}, requirements.GetPreferred()...), requirements.GetRequisite()...) {
		if zone := topology.GetSegments()[TopologyZoneKey]; zone != "" {
			candidates = append(candidates, zone)
		}
	}
	if sourceZone != "" {
		if len(candidates) == 0 {
			return sourceZone, nil
		}
		for _, candidate := range candidates {
			if candidate == sourceZone {
				return sourceZone, nil
			}
		}
		return "", status.Errorf(codes.ResourceExhausted, "the source is in zone %s, which the topology requirements do not allow", sourceZone)
	}
	if len(candidates) > 0 {
		return candidates[0], nil
	}
	if driver.options.DefaultZone != "" {
		return driver.options.DefaultZone, nil
	}
	return "", status.Errorf(codes.InvalidArgument, "no zone: the request has no %s topology and the controller has no default zone", TopologyZoneKey)
}

// nodeFromTopology is the server the scheduler picked (WaitForFirstConsumer puts it first in preferred).
func nodeFromTopology(requirements *csi.TopologyRequirement, zone string) string {
	for _, topology := range append(append([]*csi.Topology{}, requirements.GetPreferred()...), requirements.GetRequisite()...) {
		segments := topology.GetSegments()
		if node := segments[TopologyNodeKey]; node != "" && (segments[TopologyZoneKey] == "" || segments[TopologyZoneKey] == zone) {
			return node
		}
	}
	return ""
}

func (driver *Driver) csiVolume(volume cloud.Volume, node string, request *csi.CreateVolumeRequest) *csi.Volume {
	topology := map[string]string{TopologyZoneKey: volume.Zone}
	volumeContext := map[string]string{ParameterTier: volume.Tier, TopologyZoneKey: volume.Zone}
	if volume.Tier == TierLocalNVMe && node != "" {
		topology[TopologyNodeKey] = node
		volumeContext[TopologyNodeKey] = node
	}
	if filesystemType := request.GetParameters()[ParameterFilesystemType]; filesystemType != "" {
		volumeContext[ParameterFilesystemType] = filesystemType
	}
	return &csi.Volume{
		VolumeId:           volume.ID,
		CapacityBytes:      volume.SizeGibibytes * gibibyte,
		VolumeContext:      volumeContext,
		ContentSource:      request.GetVolumeContentSource(),
		AccessibleTopology: []*csi.Topology{{Segments: topology}},
	}
}

// DeleteVolume deletes the storage; a storage that is already gone is success.
func (driver *Driver) DeleteVolume(ctx context.Context, request *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := request.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	release, isLocked := driver.lock("volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	ctx, cancel := driver.operationContext(ctx)
	defer cancel()
	volume, getError := driver.options.API.GetVolume(ctx, volumeID)
	if cloud.IsNotFound(getError) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if getError != nil {
		return nil, apiStatus(getError, "read volume %s", volumeID)
	}
	if volume.ServerID != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is still attached to server %s", volumeID, volume.ServerID)
	}
	operation, deleteError := driver.options.API.DeleteVolume(ctx, volumeID)
	if cloud.IsNotFound(deleteError) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if deleteError != nil {
		return nil, apiStatus(deleteError, "delete volume %s", volumeID)
	}
	if waitError := driver.wait(ctx, operation); waitError != nil {
		return nil, apiStatus(waitError, "delete volume %s", volumeID)
	}
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerPublishVolume hot-plugs the storage into the node's server and waits for the attach operation.
func (driver *Driver) ControllerPublishVolume(ctx context.Context, request *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	volumeID, nodeID := request.GetVolumeId(), request.GetNodeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the node id is required")
	}
	if request.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "the volume capability is required")
	}
	if capabilityError := validateCapabilities([]*csi.VolumeCapability{request.GetVolumeCapability()}); capabilityError != nil {
		return nil, status.Error(codes.InvalidArgument, capabilityError.Error())
	}
	release, isLocked := driver.lock("volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	ctx, cancel := driver.operationContext(ctx)
	defer cancel()
	volume, getError := driver.options.API.GetVolume(ctx, volumeID)
	if getError != nil {
		return nil, apiStatus(getError, "read volume %s", volumeID)
	}
	if _, serverError := driver.options.API.GetServer(ctx, nodeID); serverError != nil {
		return nil, apiStatus(serverError, "read server %s", nodeID)
	}
	publishContext := map[string]string{PublishContextDeviceSerial: volume.DeviceSerial}
	if volume.DeviceSerial == "" {
		publishContext[PublishContextDeviceSerial] = cloud.DeviceSerialFor(volume.ID)
	}
	if volume.ServerID == nodeID {
		return &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}, nil
	}
	if volume.ServerID != "" {
		return nil, status.Errorf(codes.FailedPrecondition, "volume %s is attached to server %s; it is single-node", volumeID, volume.ServerID)
	}
	operation, attachError := driver.options.API.AttachVolume(ctx, volumeID, nodeID)
	if attachError != nil {
		return nil, apiStatus(attachError, "attach volume %s to server %s", volumeID, nodeID)
	}
	if waitError := driver.wait(ctx, operation); waitError != nil {
		return nil, apiStatus(waitError, "attach volume %s to server %s", volumeID, nodeID)
	}
	return &csi.ControllerPublishVolumeResponse{PublishContext: publishContext}, nil
}

// ControllerUnpublishVolume hot-unplugs the storage from the node's server. A volume that is gone or attached
// elsewhere is already unpublished from this node.
func (driver *Driver) ControllerUnpublishVolume(ctx context.Context, request *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	volumeID, nodeID := request.GetVolumeId(), request.GetNodeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	release, isLocked := driver.lock("volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	ctx, cancel := driver.operationContext(ctx)
	defer cancel()
	volume, getError := driver.options.API.GetVolume(ctx, volumeID)
	if cloud.IsNotFound(getError) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	if getError != nil {
		return nil, apiStatus(getError, "read volume %s", volumeID)
	}
	if volume.ServerID == "" || (nodeID != "" && volume.ServerID != nodeID) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	operation, detachError := driver.options.API.DetachVolume(ctx, volumeID)
	if cloud.IsNotFound(detachError) {
		return &csi.ControllerUnpublishVolumeResponse{}, nil
	}
	if detachError != nil {
		return nil, apiStatus(detachError, "detach volume %s", volumeID)
	}
	if waitError := driver.wait(ctx, operation); waitError != nil {
		return nil, apiStatus(waitError, "detach volume %s", volumeID)
	}
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ValidateVolumeCapabilities confirms single-node writer access, mount or block.
func (driver *Driver) ValidateVolumeCapabilities(ctx context.Context, request *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if request.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if len(request.GetVolumeCapabilities()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities are required")
	}
	if _, getError := driver.options.API.GetVolume(ctx, request.GetVolumeId()); getError != nil {
		return nil, apiStatus(getError, "read volume %s", request.GetVolumeId())
	}
	if capabilityError := validateCapabilities(request.GetVolumeCapabilities()); capabilityError != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{Message: capabilityError.Error()}, nil
	}
	return &csi.ValidateVolumeCapabilitiesResponse{Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
		VolumeContext:      request.GetVolumeContext(),
		VolumeCapabilities: request.GetVolumeCapabilities(),
		Parameters:         request.GetParameters(),
	}}, nil
}

// ControllerExpandVolume grows the storage online; the node then grows the file system.
func (driver *Driver) ControllerExpandVolume(ctx context.Context, request *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volumeID := request.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if request.GetCapacityRange() == nil {
		return nil, status.Error(codes.InvalidArgument, "the capacity range is required")
	}
	sizeGibibytes, sizeError := requestedGibibytes(request.GetCapacityRange())
	if sizeError != nil {
		return nil, sizeError
	}
	release, isLocked := driver.lock("volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	ctx, cancel := driver.operationContext(ctx)
	defer cancel()
	volume, getError := driver.options.API.GetVolume(ctx, volumeID)
	if getError != nil {
		return nil, apiStatus(getError, "read volume %s", volumeID)
	}
	isBlock := request.GetVolumeCapability().GetBlock() != nil
	if volume.SizeGibibytes >= sizeGibibytes {
		return &csi.ControllerExpandVolumeResponse{CapacityBytes: volume.SizeGibibytes * gibibyte, NodeExpansionRequired: !isBlock}, nil
	}
	operation, resizeError := driver.options.API.ResizeVolume(ctx, volumeID, sizeGibibytes)
	if resizeError != nil {
		return nil, apiStatus(resizeError, "resize volume %s to %d GiB", volumeID, sizeGibibytes)
	}
	if waitError := driver.wait(ctx, operation); waitError != nil {
		return nil, apiStatus(waitError, "resize volume %s to %d GiB", volumeID, sizeGibibytes)
	}
	return &csi.ControllerExpandVolumeResponse{CapacityBytes: sizeGibibytes * gibibyte, NodeExpansionRequired: !isBlock}, nil
}

// CreateSnapshot finds the snapshot named after the request or takes one. The snapshot is returned as soon as the
// API accepted it; the snapshotter calls again until it is ready to use.
func (driver *Driver) CreateSnapshot(ctx context.Context, request *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
	name, sourceID := request.GetName(), request.GetSourceVolumeId()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "the snapshot name is required")
	}
	if sourceID == "" {
		return nil, status.Error(codes.InvalidArgument, "the source volume id is required")
	}
	release, isLocked := driver.lock("snapshot/" + name)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on snapshot %s is in progress", name)
	}
	defer release()
	ctx, cancel := driver.operationContext(ctx)
	defer cancel()
	snapshots, listError := driver.allSnapshots(ctx)
	if listError != nil {
		return nil, apiStatus(listError, "list snapshots")
	}
	for _, snapshot := range snapshots {
		if snapshot.Title != name {
			continue
		}
		if snapshot.StorageID != sourceID {
			return nil, status.Errorf(codes.AlreadyExists, "snapshot %s exists for volume %s, not %s", name, snapshot.StorageID, sourceID)
		}
		return &csi.CreateSnapshotResponse{Snapshot: csiSnapshot(snapshot)}, nil
	}
	snapshot, operation, createError := driver.options.API.CreateSnapshot(ctx, sourceID, name)
	if createError != nil {
		return nil, apiStatus(createError, "snapshot volume %s", sourceID)
	}
	if operation.Status == cloud.OperationStatusFailed || operation.Status == cloud.OperationStatusCancelled {
		return nil, status.Errorf(codes.Internal, "snapshot %s of volume %s %s: %s", name, sourceID, operation.Status, operation.Error)
	}
	return &csi.CreateSnapshotResponse{Snapshot: csiSnapshot(snapshot)}, nil
}

// DeleteSnapshot deletes a snapshot; one that is already gone is success.
func (driver *Driver) DeleteSnapshot(ctx context.Context, request *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
	snapshotID := request.GetSnapshotId()
	if snapshotID == "" {
		return nil, status.Error(codes.InvalidArgument, "the snapshot id is required")
	}
	release, isLocked := driver.lock("snapshot-id/" + snapshotID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on snapshot %s is in progress", snapshotID)
	}
	defer release()
	ctx, cancel := driver.operationContext(ctx)
	defer cancel()
	operation, deleteError := driver.options.API.DeleteSnapshot(ctx, snapshotID)
	if cloud.IsNotFound(deleteError) {
		return &csi.DeleteSnapshotResponse{}, nil
	}
	if deleteError != nil {
		return nil, apiStatus(deleteError, "delete snapshot %s", snapshotID)
	}
	if waitError := driver.wait(ctx, operation); waitError != nil {
		return nil, apiStatus(waitError, "delete snapshot %s", snapshotID)
	}
	return &csi.DeleteSnapshotResponse{}, nil
}

// ListSnapshots lists one snapshot, a volume's snapshots, or all of them. The starting token is an offset into the
// list sorted by id.
func (driver *Driver) ListSnapshots(ctx context.Context, request *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
	if request.GetMaxEntries() < 0 {
		return nil, status.Error(codes.InvalidArgument, "max_entries must not be negative")
	}
	var snapshots []cloud.Snapshot
	switch {
	case request.GetSnapshotId() != "":
		snapshot, getError := driver.options.API.GetSnapshot(ctx, request.GetSnapshotId())
		if cloud.IsNotFound(getError) {
			return &csi.ListSnapshotsResponse{}, nil
		}
		if getError != nil {
			return nil, apiStatus(getError, "read snapshot %s", request.GetSnapshotId())
		}
		if request.GetSourceVolumeId() != "" && snapshot.StorageID != request.GetSourceVolumeId() {
			return &csi.ListSnapshotsResponse{}, nil
		}
		snapshots = []cloud.Snapshot{snapshot}
	case request.GetSourceVolumeId() != "":
		listed, listError := driver.options.API.ListVolumeSnapshots(ctx, request.GetSourceVolumeId())
		if cloud.IsNotFound(listError) {
			return &csi.ListSnapshotsResponse{}, nil
		}
		if listError != nil {
			return nil, apiStatus(listError, "list the snapshots of volume %s", request.GetSourceVolumeId())
		}
		snapshots = listed
	default:
		listed, listError := driver.allSnapshots(ctx)
		if listError != nil {
			return nil, apiStatus(listError, "list snapshots")
		}
		snapshots = listed
	}
	sort.Slice(snapshots, func(first, second int) bool { return snapshots[first].ID < snapshots[second].ID })
	start := 0
	if token := request.GetStartingToken(); token != "" {
		offset, parseError := strconv.Atoi(token)
		if parseError != nil || offset < 0 || offset > len(snapshots) {
			return nil, status.Errorf(codes.Aborted, "invalid starting token %q", token)
		}
		start = offset
	}
	end := len(snapshots)
	nextToken := ""
	if maximum := int(request.GetMaxEntries()); maximum > 0 && start+maximum < end {
		end = start + maximum
		nextToken = strconv.Itoa(end)
	}
	entries := make([]*csi.ListSnapshotsResponse_Entry, 0, end-start)
	for _, snapshot := range snapshots[start:end] {
		entries = append(entries, &csi.ListSnapshotsResponse_Entry{Snapshot: csiSnapshot(snapshot)})
	}
	return &csi.ListSnapshotsResponse{Entries: entries, NextToken: nextToken}, nil
}

// allSnapshots walks every storage's snapshots: the API lists snapshots per storage.
func (driver *Driver) allSnapshots(ctx context.Context) ([]cloud.Snapshot, error) {
	volumes, listError := driver.options.API.ListVolumes(ctx)
	if listError != nil {
		return nil, listError
	}
	var snapshots []cloud.Snapshot
	for _, volume := range volumes {
		listed, snapshotsError := driver.options.API.ListVolumeSnapshots(ctx, volume.ID)
		if cloud.IsNotFound(snapshotsError) {
			continue
		}
		if snapshotsError != nil {
			return nil, fmt.Errorf("list the snapshots of volume %s: %w", volume.ID, snapshotsError)
		}
		snapshots = append(snapshots, listed...)
	}
	return snapshots, nil
}

func csiSnapshot(snapshot cloud.Snapshot) *csi.Snapshot {
	converted := &csi.Snapshot{
		SnapshotId:     snapshot.ID,
		SourceVolumeId: snapshot.StorageID,
		SizeBytes:      snapshot.SizeGibibytes * gibibyte,
		ReadyToUse:     snapshot.IsReady(),
	}
	if !snapshot.CreatedAt.IsZero() {
		converted.CreationTime = timestamppb.New(snapshot.CreatedAt)
	}
	return converted
}

// requestedGibibytes rounds the required size up to whole GiB, at least 1, within the limit.
func requestedGibibytes(capacityRange *csi.CapacityRange) (int64, error) {
	required, limit := capacityRange.GetRequiredBytes(), capacityRange.GetLimitBytes()
	if required < 0 || limit < 0 {
		return 0, status.Error(codes.InvalidArgument, "capacity must not be negative")
	}
	if limit > 0 && required > limit {
		return 0, status.Errorf(codes.OutOfRange, "required %d bytes exceeds the limit of %d bytes", required, limit)
	}
	sizeGibibytes := max((required+gibibyte-1)/gibibyte, 1)
	if limit > 0 && sizeGibibytes*gibibyte > limit {
		if limit < gibibyte {
			return 0, status.Errorf(codes.OutOfRange, "volumes are whole GiB; the limit of %d bytes is less than 1 GiB", limit)
		}
		sizeGibibytes = limit / gibibyte
		if sizeGibibytes*gibibyte < required {
			return 0, status.Errorf(codes.OutOfRange, "no whole GiB size lies between %d and %d bytes", required, limit)
		}
	}
	if sizeGibibytes > maximumVolumeGibibytes {
		return 0, status.Errorf(codes.OutOfRange, "volumes are at most %d GiB", maximumVolumeGibibytes)
	}
	return sizeGibibytes, nil
}

func fitsCapacityRange(sizeGibibytes int64, capacityRange *csi.CapacityRange) bool {
	bytes := sizeGibibytes * gibibyte
	if required := capacityRange.GetRequiredBytes(); required > 0 && bytes < required {
		return false
	}
	if limit := capacityRange.GetLimitBytes(); limit > 0 && bytes > limit {
		return false
	}
	return true
}

// validateCapabilities accepts single-node writer (RWO), mount or block.
func validateCapabilities(capabilities []*csi.VolumeCapability) error {
	for _, capability := range capabilities {
		if capability.GetBlock() == nil && capability.GetMount() == nil {
			return fmt.Errorf("an access type (mount or block) is required")
		}
		if capability.GetAccessMode() == nil {
			return fmt.Errorf("an access mode is required")
		}
		switch capability.GetAccessMode().GetMode() {
		case csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER:
		default:
			return fmt.Errorf("access mode %s is not supported: Ankra Cloud volumes are single-node writer (ReadWriteOnce)",
				capability.GetAccessMode().GetMode())
		}
		if mount := capability.GetMount(); mount != nil {
			if filesystemType := mount.GetFsType(); filesystemType != "" && !isSupportedFilesystem(filesystemType) {
				return fmt.Errorf("file system %s is not supported; use ext4 or xfs", filesystemType)
			}
		}
	}
	return nil
}
