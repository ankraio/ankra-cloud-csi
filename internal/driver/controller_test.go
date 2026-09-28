package driver

import (
	"context"
	"net/http"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
	"github.com/ankraio/ankra-cloud-csi/internal/cloud/cloudfake"
)

func mountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func blockCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
	}
}

func expectCode(t *testing.T, callError error, code codes.Code) {
	t.Helper()
	if status.Code(callError) != code {
		t.Fatalf("code %s (%v), want %s", status.Code(callError), callError, code)
	}
}

func createRequest(name string, requiredBytes int64, parameters map[string]string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{Name: name, CapacityRange: &csi.CapacityRange{RequiredBytes: requiredBytes},
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}, Parameters: parameters}
}

func TestCreateVolumeRoundsUpToWholeGibibytesWithTheDefaultTier(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	response, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-a", 1500<<20, nil))
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	volume, _ := api.Volume(response.GetVolume().GetVolumeId())
	if volume.SizeGibibytes != 2 || volume.Tier != DefaultTier || volume.Zone != testZone || volume.Title != "pvc-a" {
		t.Fatalf("storage %+v", volume)
	}
	if response.GetVolume().GetCapacityBytes() != 2*gibibyte {
		t.Fatalf("capacity %d", response.GetVolume().GetCapacityBytes())
	}
	topology := response.GetVolume().GetAccessibleTopology()
	if len(topology) != 1 || topology[0].GetSegments()[TopologyZoneKey] != testZone || topology[0].GetSegments()[TopologyNodeKey] != "" {
		t.Fatalf("topology %v", topology)
	}
}

func TestCreateVolumeWithoutCapacityIsOneGibibyte(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	response, createError := csiDriver.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-small",
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}})
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	if response.GetVolume().GetCapacityBytes() != gibibyte {
		t.Fatalf("capacity %d", response.GetVolume().GetCapacityBytes())
	}
}

func TestCreateVolumeIsIdempotentByName(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	first, firstError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-same", 5*gibibyte, map[string]string{ParameterTier: "maxiops"}))
	second, secondError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-same", 5*gibibyte, map[string]string{ParameterTier: "maxiops"}))
	if firstError != nil || secondError != nil {
		t.Fatalf("CreateVolume: %v, %v", firstError, secondError)
	}
	if first.GetVolume().GetVolumeId() != second.GetVolume().GetVolumeId() || api.VolumeCount() != 1 || api.Calls("CreateVolume") != 1 {
		t.Fatalf("created %d storages in %d calls", api.VolumeCount(), api.Calls("CreateVolume"))
	}
	_, sizeError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-same", 20*gibibyte, map[string]string{ParameterTier: "maxiops"}))
	expectCode(t, sizeError, codes.AlreadyExists)
	_, tierError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-same", 5*gibibyte, map[string]string{ParameterTier: "hdd"}))
	expectCode(t, tierError, codes.AlreadyExists)
}

func TestCreateVolumeReplacesAStorageThatFailedToProvision(t *testing.T) {
	api := cloudfake.New()
	api.AddVolume(cloud.Volume{ID: "5a000000-0000-4000-8000-000000000001", Zone: testZone, Title: "pvc-broken", Tier: DefaultTier,
		State: cloud.StorageStateError, SizeGibibytes: 1})
	csiDriver := newTestDriver(t, api, newFakeHost())
	response, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-broken", gibibyte, nil))
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	if response.GetVolume().GetVolumeId() == "5a000000-0000-4000-8000-000000000001" || api.VolumeCount() != 1 {
		t.Fatalf("the failed storage was not replaced: %s", response.GetVolume().GetVolumeId())
	}
}

func TestCreateVolumeTakesTheZoneFromTheTopology(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	request := createRequest("pvc-zone", gibibyte, nil)
	request.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: "hel1"}}, {Segments: map[string]string{TopologyZoneKey: "nbg1"}}},
		Preferred: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: "nbg1"}}},
	}
	response, createError := csiDriver.CreateVolume(context.Background(), request)
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	if volume, _ := api.Volume(response.GetVolume().GetVolumeId()); volume.Zone != "nbg1" {
		t.Fatalf("zone %s, want the preferred nbg1", volume.Zone)
	}
}

func TestCreateVolumePinsLocalNVMeToTheSelectedNode(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	request := createRequest("pvc-local", gibibyte, map[string]string{ParameterTier: TierLocalNVMe})
	request.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: testServerID}}},
		Preferred: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: testServerID}}},
	}
	response, createError := csiDriver.CreateVolume(context.Background(), request)
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	segments := response.GetVolume().GetAccessibleTopology()[0].GetSegments()
	if segments[TopologyNodeKey] != testServerID || segments[TopologyZoneKey] != testZone {
		t.Fatalf("topology %v", segments)
	}

	_, missingNodeError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-local-immediate", gibibyte,
		map[string]string{ParameterTier: TierLocalNVMe}))
	expectCode(t, missingNodeError, codes.InvalidArgument)
}

func TestCreateVolumeFromASnapshot(t *testing.T) {
	api := cloudfake.New()
	api.AddSnapshot(cloud.Snapshot{ID: "5b000000-0000-4000-8000-000000000001", StorageID: "gone", Zone: "hel1", Title: "snap",
		State: cloud.StorageStateOnline, SizeGibibytes: 8})
	csiDriver := newTestDriver(t, api, newFakeHost())
	request := createRequest("pvc-restored", gibibyte, nil)
	request.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "5b000000-0000-4000-8000-000000000001"}}}
	response, createError := csiDriver.CreateVolume(context.Background(), request)
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	volume, _ := api.Volume(response.GetVolume().GetVolumeId())
	if volume.Zone != "hel1" || volume.SizeGibibytes != 8 {
		t.Fatalf("restored storage %+v, want the snapshot's zone and size", volume)
	}
	if response.GetVolume().GetContentSource().GetSnapshot().GetSnapshotId() != "5b000000-0000-4000-8000-000000000001" {
		t.Fatalf("content source %v", response.GetVolume().GetContentSource())
	}

	request.Name = "pvc-missing-snapshot"
	request.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "absent"}}}
	_, missingError := csiDriver.CreateVolume(context.Background(), request)
	expectCode(t, missingError, codes.NotFound)
}

func TestCreateVolumeMapsAPIErrors(t *testing.T) {
	for _, testCase := range []struct {
		statusCode int
		code       codes.Code
	}{
		{http.StatusUnprocessableEntity, codes.ResourceExhausted},
		{http.StatusConflict, codes.FailedPrecondition},
		{http.StatusNotFound, codes.NotFound},
		{http.StatusBadRequest, codes.InvalidArgument},
		{http.StatusUnauthorized, codes.Unauthenticated},
		{http.StatusForbidden, codes.PermissionDenied},
		{http.StatusServiceUnavailable, codes.Unavailable},
		{http.StatusInternalServerError, codes.Internal},
	} {
		api := cloudfake.New()
		api.FailWith("CreateVolume", testCase.statusCode)
		csiDriver := newTestDriver(t, api, newFakeHost())
		_, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-error", gibibyte, nil))
		expectCode(t, createError, testCase.code)
	}
}

func TestCreateVolumeRejectsBadRequests(t *testing.T) {
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	_, noNameError := csiDriver.CreateVolume(context.Background(), &csi.CreateVolumeRequest{VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}})
	expectCode(t, noNameError, codes.InvalidArgument)
	multiWriter := mountCapability()
	multiWriter.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	_, accessError := csiDriver.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-rwx",
		VolumeCapabilities: []*csi.VolumeCapability{multiWriter}})
	expectCode(t, accessError, codes.InvalidArgument)
	_, rangeError := csiDriver.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-range",
		CapacityRange: &csi.CapacityRange{RequiredBytes: 1500 << 20, LimitBytes: 1800 << 20}, VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}})
	expectCode(t, rangeError, codes.OutOfRange)
	_, hugeError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-huge", 5000*gibibyte, nil))
	expectCode(t, hugeError, codes.OutOfRange)
}

func TestCreateVolumeReportsAFailedOperation(t *testing.T) {
	api := &failingOperationAPI{API: cloudfake.New()}
	csiDriver := newTestDriver(t, api.API, newFakeHost())
	csiDriver.options.API = api
	_, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-failing", gibibyte, nil))
	expectCode(t, createError, codes.Internal)
}

// failingOperationAPI hands out create operations that are still running and then fail.
type failingOperationAPI struct {
	*cloudfake.API
}

func (api *failingOperationAPI) CreateVolume(ctx context.Context, request cloud.CreateVolumeRequest) (cloud.Volume, cloud.Operation, error) {
	volume, _, createError := api.API.CreateVolume(ctx, request)
	running := cloud.Operation{ID: "0e-running", Status: cloud.OperationStatusRunning}
	api.SetOperation(cloud.Operation{ID: running.ID, Status: cloud.OperationStatusFailed, Step: "create_disk", Error: "no space"})
	return volume, running, createError
}

func TestDeleteVolume(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	created, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-delete", gibibyte, nil))
	volumeID := created.GetVolume().GetVolumeId()
	if _, deleteError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); deleteError != nil {
		t.Fatalf("DeleteVolume: %v", deleteError)
	}
	if _, againError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID}); againError != nil {
		t.Fatalf("DeleteVolume of a deleted volume: %v", againError)
	}
	_, emptyError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{})
	expectCode(t, emptyError, codes.InvalidArgument)

	attached, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-attached", gibibyte, nil))
	if _, publishError := csiDriver.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: attached.GetVolume().GetVolumeId(), NodeId: testServerID, VolumeCapability: mountCapability()}); publishError != nil {
		t.Fatalf("ControllerPublishVolume: %v", publishError)
	}
	_, attachedError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: attached.GetVolume().GetVolumeId()})
	expectCode(t, attachedError, codes.FailedPrecondition)

	api.FailWith("GetVolume", http.StatusForbidden)
	_, forbiddenError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: volumeID})
	expectCode(t, forbiddenError, codes.PermissionDenied)
}

func TestControllerPublishAndUnpublish(t *testing.T) {
	api := cloudfake.New()
	api.AddServer(cloud.Server{ID: "01a0d058-e588-7fff-8000-000000000202", Zone: testZone, State: "running"})
	csiDriver := newTestDriver(t, api, newFakeHost())
	created, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-publish", gibibyte, nil))
	volumeID := created.GetVolume().GetVolumeId()
	publish := &csi.ControllerPublishVolumeRequest{VolumeId: volumeID, NodeId: testServerID, VolumeCapability: mountCapability()}
	response, publishError := csiDriver.ControllerPublishVolume(context.Background(), publish)
	if publishError != nil {
		t.Fatalf("ControllerPublishVolume: %v", publishError)
	}
	if serial := response.GetPublishContext()[PublishContextDeviceSerial]; serial != volumeID[:20] {
		t.Fatalf("device serial %q, want the first 20 characters of %s", serial, volumeID)
	}
	if _, againError := csiDriver.ControllerPublishVolume(context.Background(), publish); againError != nil || api.Calls("AttachVolume") != 1 {
		t.Fatalf("second publish: %v, %d attach calls", againError, api.Calls("AttachVolume"))
	}
	_, otherNodeError := csiDriver.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "01a0d058-e588-7fff-8000-000000000202", VolumeCapability: mountCapability()})
	expectCode(t, otherNodeError, codes.FailedPrecondition)
	_, unknownNodeError := csiDriver.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: "no-such-server", VolumeCapability: mountCapability()})
	expectCode(t, unknownNodeError, codes.NotFound)
	_, unknownVolumeError := csiDriver.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: "no-such-volume", NodeId: testServerID, VolumeCapability: mountCapability()})
	expectCode(t, unknownVolumeError, codes.NotFound)

	unpublish := &csi.ControllerUnpublishVolumeRequest{VolumeId: volumeID, NodeId: testServerID}
	if _, unpublishError := csiDriver.ControllerUnpublishVolume(context.Background(), unpublish); unpublishError != nil {
		t.Fatalf("ControllerUnpublishVolume: %v", unpublishError)
	}
	if volume, _ := api.Volume(volumeID); volume.ServerID != "" {
		t.Fatalf("still attached to %s", volume.ServerID)
	}
	if _, againError := csiDriver.ControllerUnpublishVolume(context.Background(), unpublish); againError != nil || api.Calls("DetachVolume") != 1 {
		t.Fatalf("second unpublish: %v, %d detach calls", againError, api.Calls("DetachVolume"))
	}
	if _, goneError := csiDriver.ControllerUnpublishVolume(context.Background(), &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "no-such-volume", NodeId: testServerID}); goneError != nil {
		t.Fatalf("unpublish of a missing volume: %v", goneError)
	}
}

func TestControllerPublishWithNoFreeSlotIsResourceExhausted(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	created, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-full", gibibyte, nil))
	api.FailWith("AttachVolume", http.StatusUnprocessableEntity)
	_, publishError := csiDriver.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: created.GetVolume().GetVolumeId(), NodeId: testServerID, VolumeCapability: mountCapability()})
	expectCode(t, publishError, codes.ResourceExhausted)
}

func TestControllerExpandVolume(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	created, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-grow", 2*gibibyte, nil))
	volumeID := created.GetVolume().GetVolumeId()
	response, expandError := csiDriver.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: volumeID, CapacityRange: &csi.CapacityRange{RequiredBytes: 4*gibibyte + 1}, VolumeCapability: mountCapability()})
	if expandError != nil {
		t.Fatalf("ControllerExpandVolume: %v", expandError)
	}
	if response.GetCapacityBytes() != 5*gibibyte || !response.GetNodeExpansionRequired() {
		t.Fatalf("response %v", response)
	}
	block, blockError := csiDriver.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: volumeID, CapacityRange: &csi.CapacityRange{RequiredBytes: 3 * gibibyte}, VolumeCapability: blockCapability()})
	if blockError != nil || block.GetNodeExpansionRequired() || block.GetCapacityBytes() != 5*gibibyte || api.Calls("ResizeVolume") != 1 {
		t.Fatalf("shrinking request: %v %v, %d resizes", block, blockError, api.Calls("ResizeVolume"))
	}
	_, missingError := csiDriver.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "no-such-volume", CapacityRange: &csi.CapacityRange{RequiredBytes: gibibyte}})
	expectCode(t, missingError, codes.NotFound)
	_, noRangeError := csiDriver.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{VolumeId: volumeID})
	expectCode(t, noRangeError, codes.InvalidArgument)
	api.FailWith("ResizeVolume", http.StatusConflict)
	_, conflictError := csiDriver.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: volumeID, CapacityRange: &csi.CapacityRange{RequiredBytes: 9 * gibibyte}})
	expectCode(t, conflictError, codes.FailedPrecondition)
}

func TestValidateVolumeCapabilities(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	created, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-validate", gibibyte, nil))
	volumeID := created.GetVolume().GetVolumeId()
	confirmed, validateError := csiDriver.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: volumeID, VolumeCapabilities: []*csi.VolumeCapability{mountCapability(), blockCapability()}})
	if validateError != nil || confirmed.GetConfirmed() == nil {
		t.Fatalf("RWO: %v %v", confirmed, validateError)
	}
	readMany := mountCapability()
	readMany.AccessMode.Mode = csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	refused, refusedError := csiDriver.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: volumeID, VolumeCapabilities: []*csi.VolumeCapability{readMany}})
	if refusedError != nil || refused.GetConfirmed() != nil || refused.GetMessage() == "" {
		t.Fatalf("ROX: %v %v", refused, refusedError)
	}
	_, missingError := csiDriver.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "no-such-volume", VolumeCapabilities: []*csi.VolumeCapability{mountCapability()}})
	expectCode(t, missingError, codes.NotFound)
}

func TestSnapshots(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	first, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-snap-a", 3*gibibyte, nil))
	second, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-snap-b", gibibyte, nil))
	firstID, secondID := first.GetVolume().GetVolumeId(), second.GetVolume().GetVolumeId()

	created, createError := csiDriver.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-a", SourceVolumeId: firstID})
	if createError != nil {
		t.Fatalf("CreateSnapshot: %v", createError)
	}
	snapshot := created.GetSnapshot()
	if snapshot.GetSourceVolumeId() != firstID || snapshot.GetSizeBytes() != 3*gibibyte || !snapshot.GetReadyToUse() || snapshot.GetCreationTime() == nil {
		t.Fatalf("snapshot %v", snapshot)
	}
	again, againError := csiDriver.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-a", SourceVolumeId: firstID})
	if againError != nil || again.GetSnapshot().GetSnapshotId() != snapshot.GetSnapshotId() || api.Calls("CreateSnapshot") != 1 {
		t.Fatalf("second CreateSnapshot: %v %v", again, againError)
	}
	_, otherSourceError := csiDriver.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-a", SourceVolumeId: secondID})
	expectCode(t, otherSourceError, codes.AlreadyExists)
	_, missingSourceError := csiDriver.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-x", SourceVolumeId: "no-such-volume"})
	expectCode(t, missingSourceError, codes.NotFound)
	if _, secondSnapshotError := csiDriver.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{Name: "snap-b", SourceVolumeId: secondID}); secondSnapshotError != nil {
		t.Fatalf("CreateSnapshot: %v", secondSnapshotError)
	}

	all, _ := csiDriver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{})
	if len(all.GetEntries()) != 2 {
		t.Fatalf("listed %d snapshots, want 2", len(all.GetEntries()))
	}
	page, _ := csiDriver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{MaxEntries: 1})
	if len(page.GetEntries()) != 1 || page.GetNextToken() != "1" {
		t.Fatalf("first page %v", page)
	}
	rest, _ := csiDriver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: page.GetNextToken()})
	if len(rest.GetEntries()) != 1 || rest.GetEntries()[0].GetSnapshot().GetSnapshotId() == page.GetEntries()[0].GetSnapshot().GetSnapshotId() {
		t.Fatalf("second page %v", rest)
	}
	_, tokenError := csiDriver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{StartingToken: "nonsense"})
	expectCode(t, tokenError, codes.Aborted)
	bySource, _ := csiDriver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SourceVolumeId: secondID})
	if len(bySource.GetEntries()) != 1 || bySource.GetEntries()[0].GetSnapshot().GetSourceVolumeId() != secondID {
		t.Fatalf("by source %v", bySource)
	}
	byID, _ := csiDriver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: snapshot.GetSnapshotId()})
	if len(byID.GetEntries()) != 1 {
		t.Fatalf("by id %v", byID)
	}
	missing, missingError := csiDriver.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{SnapshotId: "no-such-snapshot"})
	if missingError != nil || len(missing.GetEntries()) != 0 {
		t.Fatalf("missing id: %v %v", missing, missingError)
	}

	if _, deleteError := csiDriver.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: snapshot.GetSnapshotId()}); deleteError != nil {
		t.Fatalf("DeleteSnapshot: %v", deleteError)
	}
	if _, againError := csiDriver.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: snapshot.GetSnapshotId()}); againError != nil {
		t.Fatalf("DeleteSnapshot of a deleted snapshot: %v", againError)
	}
	api.FailWith("DeleteSnapshot", http.StatusConflict)
	_, conflictError := csiDriver.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{SnapshotId: "any"})
	expectCode(t, conflictError, codes.FailedPrecondition)
}

func TestIdentityService(t *testing.T) {
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	info, _ := csiDriver.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	if info.GetName() != "csi.ankra.cloud" || info.GetVendorVersion() != "test" {
		t.Fatalf("plugin info %v", info)
	}
	capabilities, _ := csiDriver.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	var hasController, hasTopology, hasOnlineExpansion bool
	for _, capability := range capabilities.GetCapabilities() {
		switch capability.GetService().GetType() {
		case csi.PluginCapability_Service_CONTROLLER_SERVICE:
			hasController = true
		case csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS:
			hasTopology = true
		}
		if capability.GetVolumeExpansion().GetType() == csi.PluginCapability_VolumeExpansion_ONLINE {
			hasOnlineExpansion = true
		}
	}
	if !hasController || !hasTopology || !hasOnlineExpansion {
		t.Fatalf("capabilities %v", capabilities)
	}
	probe, _ := csiDriver.Probe(context.Background(), &csi.ProbeRequest{})
	if !probe.GetReady().GetValue() {
		t.Fatal("not ready")
	}
}

func TestCreateVolumeClonesAVolume(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	source, _ := csiDriver.CreateVolume(context.Background(), createRequest("pvc-source", 6*gibibyte, nil))
	request := createRequest("pvc-clone", gibibyte, nil)
	request.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Volume{
		Volume: &csi.VolumeContentSource_VolumeSource{VolumeId: source.GetVolume().GetVolumeId()}}}
	clone, cloneError := csiDriver.CreateVolume(context.Background(), request)
	if cloneError != nil {
		t.Fatalf("CreateVolume clone: %v", cloneError)
	}
	if clone.GetVolume().GetCapacityBytes() != 6*gibibyte {
		t.Fatalf("clone capacity %d, want the source's 6 GiB", clone.GetVolume().GetCapacityBytes())
	}
	request.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: "elsewhere"}}}}
	request.Name = "pvc-clone-elsewhere"
	_, zoneError := csiDriver.CreateVolume(context.Background(), request)
	expectCode(t, zoneError, codes.ResourceExhausted)
}

func TestControllerGetCapabilities(t *testing.T) {
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	response, _ := csiDriver.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if len(response.GetCapabilities()) != len(controllerCapabilities) {
		t.Fatalf("capabilities %v", response.GetCapabilities())
	}
}

func TestNewRefusesIncompleteOptions(t *testing.T) {
	if _, newError := New(Options{Mode: ModeController}); newError == nil {
		t.Fatal("no API accepted")
	}
	if _, newError := New(Options{Mode: ModeNode, API: cloudfake.New()}); newError == nil {
		t.Fatal("node mode without a host accepted")
	}
	if _, newError := New(Options{Mode: "sideways", API: cloudfake.New()}); newError == nil {
		t.Fatal("unknown mode accepted")
	}
}
