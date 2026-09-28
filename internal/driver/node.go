package driver

import (
	"context"
	"errors"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// DefaultFilesystemType is used when neither the capability nor the StorageClass names one.
const DefaultFilesystemType = "ext4"

func isSupportedFilesystem(filesystemType string) bool {
	switch strings.ToLower(filesystemType) {
	case "ext4", "xfs":
		return true
	}
	return false
}

var nodeCapabilities = []csi.NodeServiceCapability_RPC_Type{
	csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
	csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
	csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
}

// NodeGetCapabilities lists the node RPCs the driver implements.
func (driver *Driver) NodeGetCapabilities(context.Context, *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	capabilities := make([]*csi.NodeServiceCapability, 0, len(nodeCapabilities))
	for _, capability := range nodeCapabilities {
		capabilities = append(capabilities, &csi.NodeServiceCapability{Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{Type: capability}}})
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: capabilities}, nil
}

// nodeIdentity resolves the node's server id and zone once.
func (driver *Driver) nodeIdentity(ctx context.Context) (NodeIdentity, error) {
	driver.identityOnce.Lock()
	defer driver.identityOnce.Unlock()
	if driver.identity != nil {
		return *driver.identity, nil
	}
	identity, resolveError := driver.options.ResolveNodeIdentity(ctx)
	if resolveError != nil {
		return NodeIdentity{}, resolveError
	}
	if identity.ServerID == "" || identity.Zone == "" {
		return NodeIdentity{}, errors.New("the node identity needs a server id and a zone")
	}
	driver.identity = &identity
	return identity, nil
}

// NodeGetInfo: the node id is the server id; the node carries its zone and its own node topology.
func (driver *Driver) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	identity, identityError := driver.nodeIdentity(ctx)
	if identityError != nil {
		return nil, status.Errorf(codes.Unavailable, "resolve the node identity: %v", identityError)
	}
	return &csi.NodeGetInfoResponse{
		NodeId:            identity.ServerID,
		MaxVolumesPerNode: driver.options.MaximumVolumesPerNode,
		AccessibleTopology: &csi.Topology{Segments: map[string]string{
			TopologyZoneKey: identity.Zone,
			TopologyNodeKey: identity.ServerID,
		}},
	}, nil
}

func deviceSerial(volumeID string, publishContext map[string]string) string {
	if serial := publishContext[PublishContextDeviceSerial]; serial != "" {
		return serial
	}
	return cloud.DeviceSerialFor(volumeID)
}

func (driver *Driver) findDevice(ctx context.Context, volumeID string, publishContext map[string]string) (string, error) {
	device, findError := driver.options.Host.FindDevice(ctx, deviceSerial(volumeID, publishContext), driver.options.DeviceWaitTimeout)
	if errors.Is(findError, ErrDeviceNotFound) {
		return "", status.Errorf(codes.NotFound, "volume %s: %v", volumeID, findError)
	}
	if findError != nil {
		return "", status.Errorf(codes.Internal, "volume %s: %v", volumeID, findError)
	}
	return device, nil
}

func filesystemTypeFor(capability *csi.VolumeCapability, volumeContext map[string]string) string {
	if filesystemType := capability.GetMount().GetFsType(); filesystemType != "" {
		return strings.ToLower(filesystemType)
	}
	if filesystemType := volumeContext[ParameterFilesystemType]; filesystemType != "" {
		return strings.ToLower(filesystemType)
	}
	return DefaultFilesystemType
}

// NodeStageVolume formats the hot-plugged disk when it is blank and mounts it at the staging path. A block volume
// has nothing to stage beyond its disk being present.
func (driver *Driver) NodeStageVolume(ctx context.Context, request *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	volumeID, stagingPath, capability := request.GetVolumeId(), request.GetStagingTargetPath(), request.GetVolumeCapability()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "the staging target path is required")
	}
	if capability == nil {
		return nil, status.Error(codes.InvalidArgument, "the volume capability is required")
	}
	if capabilityError := validateCapabilities([]*csi.VolumeCapability{capability}); capabilityError != nil {
		return nil, status.Error(codes.InvalidArgument, capabilityError.Error())
	}
	filesystemType := filesystemTypeFor(capability, request.GetVolumeContext())
	if capability.GetMount() != nil && !isSupportedFilesystem(filesystemType) {
		return nil, status.Errorf(codes.InvalidArgument, "file system %s is not supported; use ext4 or xfs", filesystemType)
	}
	release, isLocked := driver.lock("node-volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	device, findError := driver.findDevice(ctx, volumeID, request.GetPublishContext())
	if findError != nil {
		return nil, findError
	}
	if capability.GetBlock() != nil {
		return &csi.NodeStageVolumeResponse{}, nil
	}
	isMounted, checkError := driver.options.Host.IsMountPoint(stagingPath)
	if checkError != nil {
		return nil, status.Errorf(codes.Internal, "check the staging path %s: %v", stagingPath, checkError)
	}
	if isMounted {
		return &csi.NodeStageVolumeResponse{}, nil
	}
	if mkdirError := driver.options.Host.MakeDirectory(stagingPath); mkdirError != nil {
		return nil, status.Errorf(codes.Internal, "create the staging path %s: %v", stagingPath, mkdirError)
	}
	options := append([]string{"defaults"}, capability.GetMount().GetMountFlags()...)
	if mountError := driver.options.Host.FormatAndMount(device, stagingPath, filesystemType, options); mountError != nil {
		return nil, status.Errorf(codes.Internal, "format and mount %s (%s) at %s: %v", device, filesystemType, stagingPath, mountError)
	}
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodeUnstageVolume unmounts the staging path.
func (driver *Driver) NodeUnstageVolume(_ context.Context, request *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	volumeID, stagingPath := request.GetVolumeId(), request.GetStagingTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "the staging target path is required")
	}
	release, isLocked := driver.lock("node-volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	isMounted, checkError := driver.options.Host.IsMountPoint(stagingPath)
	if checkError != nil {
		return nil, status.Errorf(codes.Internal, "check the staging path %s: %v", stagingPath, checkError)
	}
	if isMounted {
		if unmountError := driver.options.Host.Unmount(stagingPath); unmountError != nil {
			return nil, status.Errorf(codes.Internal, "unmount %s: %v", stagingPath, unmountError)
		}
	}
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodePublishVolume bind-mounts the staged file system (or the block device) at the target path, read-only when asked.
func (driver *Driver) NodePublishVolume(ctx context.Context, request *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	volumeID, targetPath, stagingPath, capability := request.GetVolumeId(), request.GetTargetPath(), request.GetStagingTargetPath(), request.GetVolumeCapability()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is required")
	}
	if capability == nil {
		return nil, status.Error(codes.InvalidArgument, "the volume capability is required")
	}
	if capabilityError := validateCapabilities([]*csi.VolumeCapability{capability}); capabilityError != nil {
		return nil, status.Error(codes.InvalidArgument, capabilityError.Error())
	}
	if capability.GetMount() != nil && stagingPath == "" {
		return nil, status.Error(codes.FailedPrecondition, "the staging target path is required: the volume is staged first")
	}
	release, isLocked := driver.lock("node-volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	host := driver.options.Host
	options := []string{"bind"}
	if request.GetReadonly() {
		options = append(options, "ro")
	}
	source := stagingPath
	if capability.GetBlock() != nil {
		device, findError := driver.findDevice(ctx, volumeID, request.GetPublishContext())
		if findError != nil {
			return nil, findError
		}
		source = device
		if makeError := host.MakeFile(targetPath); makeError != nil {
			return nil, status.Errorf(codes.Internal, "create the target file %s: %v", targetPath, makeError)
		}
	} else {
		options = append(options, capability.GetMount().GetMountFlags()...)
		if mkdirError := host.MakeDirectory(targetPath); mkdirError != nil {
			return nil, status.Errorf(codes.Internal, "create the target path %s: %v", targetPath, mkdirError)
		}
	}
	isMounted, checkError := host.IsMountPoint(targetPath)
	if checkError != nil {
		return nil, status.Errorf(codes.Internal, "check the target path %s: %v", targetPath, checkError)
	}
	if isMounted {
		return &csi.NodePublishVolumeResponse{}, nil
	}
	if mountError := host.Mount(source, targetPath, "", options); mountError != nil {
		return nil, status.Errorf(codes.Internal, "bind mount %s at %s: %v", source, targetPath, mountError)
	}
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts and removes the target path.
func (driver *Driver) NodeUnpublishVolume(_ context.Context, request *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeID, targetPath := request.GetVolumeId(), request.GetTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "the target path is required")
	}
	release, isLocked := driver.lock("node-volume/" + volumeID)
	if !isLocked {
		return nil, status.Errorf(codes.Aborted, "an operation on volume %s is in progress", volumeID)
	}
	defer release()
	if cleanupError := driver.options.Host.CleanupMountPoint(targetPath); cleanupError != nil {
		return nil, status.Errorf(codes.Internal, "unmount %s: %v", targetPath, cleanupError)
	}
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeExpandVolume grows the file system to the disk's new size; a block volume needs nothing.
func (driver *Driver) NodeExpandVolume(ctx context.Context, request *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	volumeID, volumePath := request.GetVolumeId(), request.GetVolumePath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume path is required")
	}
	host := driver.options.Host
	exists, existsError := host.PathExists(volumePath)
	if existsError != nil {
		return nil, status.Errorf(codes.Internal, "check %s: %v", volumePath, existsError)
	}
	if !exists {
		return nil, status.Errorf(codes.NotFound, "volume %s is not at %s", volumeID, volumePath)
	}
	capacity := request.GetCapacityRange().GetRequiredBytes()
	if request.GetVolumeCapability().GetBlock() != nil {
		return &csi.NodeExpandVolumeResponse{CapacityBytes: capacity}, nil
	}
	if isBlock, _ := host.IsBlockDevice(volumePath); isBlock {
		return &csi.NodeExpandVolumeResponse{CapacityBytes: capacity}, nil
	}
	device, findError := driver.findDevice(ctx, volumeID, nil)
	if findError != nil {
		return nil, findError
	}
	if resizeError := host.ResizeFilesystem(device, volumePath); resizeError != nil {
		return nil, status.Errorf(codes.Internal, "grow the file system of %s at %s: %v", device, volumePath, resizeError)
	}
	return &csi.NodeExpandVolumeResponse{CapacityBytes: capacity}, nil
}

// NodeGetVolumeStats reports bytes and inodes of a mounted volume, or the size of a block volume.
func (driver *Driver) NodeGetVolumeStats(_ context.Context, request *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumeID, volumePath := request.GetVolumeId(), request.GetVolumePath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume id is required")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "the volume path is required")
	}
	host := driver.options.Host
	exists, existsError := host.PathExists(volumePath)
	if existsError != nil {
		return nil, status.Errorf(codes.Internal, "check %s: %v", volumePath, existsError)
	}
	if !exists {
		return nil, status.Errorf(codes.NotFound, "volume %s is not at %s", volumeID, volumePath)
	}
	isBlock, blockError := host.IsBlockDevice(volumePath)
	if blockError != nil {
		return nil, status.Errorf(codes.Internal, "inspect %s: %v", volumePath, blockError)
	}
	if isBlock {
		size, sizeError := host.BlockDeviceSizeBytes(volumePath)
		if sizeError != nil {
			return nil, status.Errorf(codes.Internal, "size of %s: %v", volumePath, sizeError)
		}
		return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{{Unit: csi.VolumeUsage_BYTES, Total: size}}}, nil
	}
	statistics, statsError := host.FilesystemStats(volumePath)
	if statsError != nil {
		return nil, status.Errorf(codes.Internal, "statistics of %s: %v", volumePath, statsError)
	}
	return &csi.NodeGetVolumeStatsResponse{Usage: []*csi.VolumeUsage{
		{Unit: csi.VolumeUsage_BYTES, Total: statistics.TotalBytes, Available: statistics.AvailableBytes, Used: statistics.UsedBytes},
		{Unit: csi.VolumeUsage_INODES, Total: statistics.TotalInodes, Available: statistics.FreeInodes, Used: statistics.UsedInodes},
	}}, nil
}
