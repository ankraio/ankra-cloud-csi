package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
	"github.com/ankraio/ankra-cloud-csi/internal/cloud/cloudfake"
)

const nodeVolumeID = "5a000000-0000-4000-8000-000000000042"

func TestNodeGetInfo(t *testing.T) {
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	info, infoError := csiDriver.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if infoError != nil {
		t.Fatalf("NodeGetInfo: %v", infoError)
	}
	segments := info.GetAccessibleTopology().GetSegments()
	if info.GetNodeId() != testServerID || info.GetMaxVolumesPerNode() != 15 || segments[TopologyZoneKey] != testZone || segments[TopologyNodeKey] != testServerID {
		t.Fatalf("node info %v", info)
	}

	failing := newTestDriver(t, cloudfake.New(), newFakeHost())
	failing.options.ResolveNodeIdentity = func(context.Context) (NodeIdentity, error) {
		return NodeIdentity{}, errors.New("metadata service unreachable")
	}
	_, failingError := failing.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	expectCode(t, failingError, codes.Unavailable)
}

func TestNodeStageFormatsTheVirtioDiskAndIsIdempotent(t *testing.T) {
	host := newFakeHost()
	csiDriver := newTestDriver(t, cloudfake.New(), host)
	stagingPath := filepath.Join(t.TempDir(), "staging")
	request := &csi.NodeStageVolumeRequest{VolumeId: nodeVolumeID, StagingTargetPath: stagingPath, VolumeCapability: mountCapability()}
	if _, stageError := csiDriver.NodeStageVolume(context.Background(), request); stageError != nil {
		t.Fatalf("NodeStageVolume: %v", stageError)
	}
	mounted, isMounted := host.mountAt(stagingPath)
	if !isMounted || mounted.source != "/dev/fake-"+cloud.DeviceSerialFor(nodeVolumeID) || mounted.filesystemType != "ext4" {
		t.Fatalf("staging mount %+v %v", mounted, isMounted)
	}
	if _, againError := csiDriver.NodeStageVolume(context.Background(), request); againError != nil {
		t.Fatalf("second NodeStageVolume: %v", againError)
	}
	if _, unstageError := csiDriver.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: nodeVolumeID, StagingTargetPath: stagingPath}); unstageError != nil {
		t.Fatalf("NodeUnstageVolume: %v", unstageError)
	}
	if _, isStillMounted := host.mountAt(stagingPath); isStillMounted {
		t.Fatal("still mounted after unstage")
	}
	if _, againError := csiDriver.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId: nodeVolumeID, StagingTargetPath: stagingPath}); againError != nil {
		t.Fatalf("second NodeUnstageVolume: %v", againError)
	}
}

func TestNodeStageHonoursTheFilesystemType(t *testing.T) {
	host := newFakeHost()
	csiDriver := newTestDriver(t, cloudfake.New(), host)
	xfsFromClass := filepath.Join(t.TempDir(), "xfs-class")
	if _, stageError := csiDriver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: nodeVolumeID,
		StagingTargetPath: xfsFromClass, VolumeCapability: mountCapability(), VolumeContext: map[string]string{ParameterFilesystemType: "xfs"}}); stageError != nil {
		t.Fatalf("NodeStageVolume: %v", stageError)
	}
	if mounted, _ := host.mountAt(xfsFromClass); mounted.filesystemType != "xfs" {
		t.Fatalf("file system %q, want xfs", mounted.filesystemType)
	}
	btrfs := mountCapability()
	btrfs.GetMount().FsType = "btrfs"
	_, btrfsError := csiDriver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: "5a000000-0000-4000-8000-000000000043",
		StagingTargetPath: filepath.Join(t.TempDir(), "btrfs"), VolumeCapability: btrfs})
	expectCode(t, btrfsError, codes.InvalidArgument)
}

func TestNodeStageWithoutTheDiskIsNotFound(t *testing.T) {
	host := newFakeHost()
	host.missingSerials[cloud.DeviceSerialFor(nodeVolumeID)] = true
	csiDriver := newTestDriver(t, cloudfake.New(), host)
	_, stageError := csiDriver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: nodeVolumeID,
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"), VolumeCapability: mountCapability()})
	expectCode(t, stageError, codes.NotFound)
}

func TestNodeStageReportsAMountFailure(t *testing.T) {
	host := newFakeHost()
	host.mountError = errors.New("wrong fs type")
	csiDriver := newTestDriver(t, cloudfake.New(), host)
	_, stageError := csiDriver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: nodeVolumeID,
		StagingTargetPath: filepath.Join(t.TempDir(), "staging"), VolumeCapability: mountCapability()})
	expectCode(t, stageError, codes.Internal)
}

func TestNodePublishBindMountsReadOnlyAndUnpublishRemovesTheTarget(t *testing.T) {
	host := newFakeHost()
	csiDriver := newTestDriver(t, cloudfake.New(), host)
	directory := t.TempDir()
	stagingPath, targetPath := filepath.Join(directory, "staging"), filepath.Join(directory, "pods", "target")
	if _, stageError := csiDriver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: nodeVolumeID,
		StagingTargetPath: stagingPath, VolumeCapability: mountCapability()}); stageError != nil {
		t.Fatalf("NodeStageVolume: %v", stageError)
	}
	publish := &csi.NodePublishVolumeRequest{VolumeId: nodeVolumeID, StagingTargetPath: stagingPath, TargetPath: targetPath,
		VolumeCapability: mountCapability(), Readonly: true}
	if _, publishError := csiDriver.NodePublishVolume(context.Background(), publish); publishError != nil {
		t.Fatalf("NodePublishVolume: %v", publishError)
	}
	mounted, isMounted := host.mountAt(targetPath)
	if !isMounted || mounted.source != stagingPath || !slices.Contains(mounted.options, "bind") || !slices.Contains(mounted.options, "ro") {
		t.Fatalf("target mount %+v", mounted)
	}
	if _, againError := csiDriver.NodePublishVolume(context.Background(), publish); againError != nil {
		t.Fatalf("second NodePublishVolume: %v", againError)
	}
	if _, unpublishError := csiDriver.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: nodeVolumeID, TargetPath: targetPath}); unpublishError != nil {
		t.Fatalf("NodeUnpublishVolume: %v", unpublishError)
	}
	if _, statError := os.Stat(targetPath); !errors.Is(statError, os.ErrNotExist) {
		t.Fatalf("target path left behind: %v", statError)
	}
	if _, againError := csiDriver.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: nodeVolumeID, TargetPath: targetPath}); againError != nil {
		t.Fatalf("second NodeUnpublishVolume: %v", againError)
	}
	_, noStagingError := csiDriver.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: nodeVolumeID,
		TargetPath: targetPath, VolumeCapability: mountCapability()})
	expectCode(t, noStagingError, codes.FailedPrecondition)
}

func TestNodeBlockVolumeIsTheRawDevice(t *testing.T) {
	host := newFakeHost()
	csiDriver := newTestDriver(t, cloudfake.New(), host)
	directory := t.TempDir()
	stagingPath, targetPath := filepath.Join(directory, "staging"), filepath.Join(directory, "pods", "block")
	if _, stageError := csiDriver.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{VolumeId: nodeVolumeID,
		StagingTargetPath: stagingPath, VolumeCapability: blockCapability()}); stageError != nil {
		t.Fatalf("NodeStageVolume: %v", stageError)
	}
	if _, isMounted := host.mountAt(stagingPath); isMounted || len(host.formatted) != 0 {
		t.Fatal("a block volume was formatted or mounted at the staging path")
	}
	publishContext := map[string]string{PublishContextDeviceSerial: "custom-serial"}
	if _, publishError := csiDriver.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{VolumeId: nodeVolumeID,
		StagingTargetPath: stagingPath, TargetPath: targetPath, VolumeCapability: blockCapability(), PublishContext: publishContext}); publishError != nil {
		t.Fatalf("NodePublishVolume: %v", publishError)
	}
	mounted, _ := host.mountAt(targetPath)
	if mounted.source != "/dev/fake-custom-serial" {
		t.Fatalf("block source %q, want the device of the published serial", mounted.source)
	}
	if info, statError := os.Stat(targetPath); statError != nil || info.IsDir() {
		t.Fatalf("block target must be a file: %v", statError)
	}
	stats, statsError := csiDriver.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: nodeVolumeID, VolumePath: targetPath})
	if statsError != nil || len(stats.GetUsage()) != 1 || stats.GetUsage()[0].GetTotal() != 10<<30 {
		t.Fatalf("block stats %v %v", stats, statsError)
	}
	expanded, expandError := csiDriver.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: nodeVolumeID,
		VolumePath: targetPath, CapacityRange: &csi.CapacityRange{RequiredBytes: 12 << 30}, VolumeCapability: blockCapability()})
	if expandError != nil || expanded.GetCapacityBytes() != 12<<30 || len(host.resized) != 0 {
		t.Fatalf("block expand %v %v, resized %v", expanded, expandError, host.resized)
	}
}

func TestNodeExpandGrowsTheFilesystem(t *testing.T) {
	host := newFakeHost()
	csiDriver := newTestDriver(t, cloudfake.New(), host)
	volumePath := t.TempDir()
	if _, expandError := csiDriver.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: nodeVolumeID,
		VolumePath: volumePath, CapacityRange: &csi.CapacityRange{RequiredBytes: 5 << 30}}); expandError != nil {
		t.Fatalf("NodeExpandVolume: %v", expandError)
	}
	if len(host.resized) != 1 || host.resized[0] != "/dev/fake-"+cloud.DeviceSerialFor(nodeVolumeID)+"@"+volumePath {
		t.Fatalf("resized %v", host.resized)
	}
	_, missingError := csiDriver.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: nodeVolumeID, VolumePath: "/no/such/path"})
	expectCode(t, missingError, codes.NotFound)
	_, noPathError := csiDriver.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{VolumeId: nodeVolumeID})
	expectCode(t, noPathError, codes.InvalidArgument)
}

func TestNodeGetVolumeStats(t *testing.T) {
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	stats, statsError := csiDriver.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: nodeVolumeID, VolumePath: t.TempDir()})
	if statsError != nil || len(stats.GetUsage()) != 2 {
		t.Fatalf("stats %v %v", stats, statsError)
	}
	bytes, inodes := stats.GetUsage()[0], stats.GetUsage()[1]
	if bytes.GetUnit() != csi.VolumeUsage_BYTES || bytes.GetTotal() != 10<<30 || inodes.GetUnit() != csi.VolumeUsage_INODES || inodes.GetUsed() != 360 {
		t.Fatalf("usage %v", stats.GetUsage())
	}
	_, missingError := csiDriver.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumeId: nodeVolumeID, VolumePath: "/no/such/path"})
	expectCode(t, missingError, codes.NotFound)
	_, noIDError := csiDriver.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{VolumePath: "/tmp"})
	expectCode(t, noIDError, codes.InvalidArgument)
}

func TestNodeCapabilities(t *testing.T) {
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	response, _ := csiDriver.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	var kinds []csi.NodeServiceCapability_RPC_Type
	for _, capability := range response.GetCapabilities() {
		kinds = append(kinds, capability.GetRpc().GetType())
	}
	for _, expected := range nodeCapabilities {
		if !slices.Contains(kinds, expected) {
			t.Fatalf("capabilities %v miss %v", kinds, expected)
		}
	}
}
