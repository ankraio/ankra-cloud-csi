package driver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

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

func TestCreateVolumeRoundsUpToWholeGibibytesWithTheZoneDefaultTier(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	response, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-a", 1500<<20, nil))
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	volume, _ := api.Volume(response.GetVolume().GetVolumeId())
	if volume.SizeGibibytes != 2 || volume.Tier != cloudfake.DefaultStorageTier || volume.Zone != testZone || volume.Title != "pvc-a" {
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

// A call that finds a storage in `error` deletes it and creates nothing: Internal ends provisioning for a claim that is
// gone, and a claim that is still there is asked about again, which is the call that creates its storage.
func TestCreateVolumeDeletesAStorageThatFailedToProvisionAndCreatesItsReplacementOnlyInALaterCall(t *testing.T) {
	api := cloudfake.New()
	failed := storageNamed("pvc-broken", 1, cloud.StorageStateError)
	api.AddVolume(failed)
	csiDriver := newTestDriver(t, api, newFakeHost())
	_, cleanupError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-broken", gibibyte, nil))
	expectCode(t, cleanupError, codes.Internal)
	expectStorages(t, api, "pvc-broken", 0)
	if api.Calls("CreateVolume") != 0 {
		t.Fatalf("the call that deleted the failed storage created %d storages", api.Calls("CreateVolume"))
	}
	response, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-broken", gibibyte, nil))
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	if created := expectStorages(t, api, "pvc-broken", 1); created[0].ID == failed.ID || created[0].ID != response.GetVolume().GetVolumeId() {
		t.Fatalf("the later call returned %s, the storages are %+v", response.GetVolume().GetVolumeId(), created)
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

func TestCreateVolumePlacesLocalNVMeNextToTheSelectedServer(t *testing.T) {
	const otherServerID = "01a0d058-e588-7fff-8000-000000000202"
	api := cloudfake.New()
	api.AddServer(cloud.Server{ID: otherServerID, Zone: testZone, State: "running"})
	api.PlaceServer(testServerID, "node-selected")
	api.PlaceServer(otherServerID, "node-other")
	csiDriver := newTestDriver(t, api, newFakeHost())
	request := createRequest("pvc-local-placed", gibibyte, map[string]string{ParameterTier: TierLocalNVMe})
	request.AccessibilityRequirements = localTopology(testServerID)
	response, createError := csiDriver.CreateVolume(context.Background(), request)
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	volumeID := response.GetVolume().GetVolumeId()
	if node := api.LocalVolumeNode(volumeID); node != "node-selected" {
		t.Fatalf("the storage landed on %s, not next to the selected server", node)
	}
	if _, publishError := csiDriver.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
		VolumeId: volumeID, NodeId: testServerID, VolumeCapability: mountCapability()}); publishError != nil {
		t.Fatalf("ControllerPublishVolume on the selected server: %v", publishError)
	}
}

func TestCreateVolumeWithoutATierUsesTheZoneDefaultAndPinsALocalDefault(t *testing.T) {
	api := cloudfake.New()
	api.SetZoneDefaultTier(testZone, TierLocalNVMe)
	api.PlaceServer(testServerID, "node-selected")
	csiDriver := newTestDriver(t, api, newFakeHost())
	request := createRequest("pvc-zone-default", gibibyte, nil)
	request.AccessibilityRequirements = localTopology(testServerID)
	response, createError := csiDriver.CreateVolume(context.Background(), request)
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	volume := response.GetVolume()
	if volume.GetVolumeContext()[ParameterTier] != TierLocalNVMe || volume.GetAccessibleTopology()[0].GetSegments()[TopologyNodeKey] != testServerID {
		t.Fatalf("volume %v", volume)
	}
	if node := api.LocalVolumeNode(volume.GetVolumeId()); node != "node-selected" {
		t.Fatalf("the zone-default local storage landed on %s", node)
	}
	again, againError := csiDriver.CreateVolume(context.Background(), request)
	if againError != nil || again.GetVolume().GetVolumeId() != volume.GetVolumeId() {
		t.Fatalf("a repeated zone-default request: %v, %v", again, againError)
	}

	standardRequest := createRequest("pvc-zone-standard", gibibyte, nil)
	standardRequest.AccessibilityRequirements = localTopology(testServerID)
	api.SetZoneDefaultTier(testZone, cloudfake.DefaultStorageTier)
	standard, standardError := csiDriver.CreateVolume(context.Background(), standardRequest)
	if standardError != nil {
		t.Fatalf("CreateVolume: %v", standardError)
	}
	if segments := standard.GetVolume().GetAccessibleTopology()[0].GetSegments(); segments[TopologyNodeKey] != "" ||
		standard.GetVolume().GetVolumeContext()[ParameterTier] != cloudfake.DefaultStorageTier {
		t.Fatalf("a zone-default standard volume %v", standard.GetVolume())
	}
}

func TestPlacementServerIsOnlyForNewLocalOrZoneDefaultVolumes(t *testing.T) {
	cases := []struct {
		tier   string
		source volumeSource
		want   string
	}{
		{tier: TierLocalNVMe, want: testServerID},
		{tier: "", want: testServerID},
		{tier: "standard", want: ""},
		{tier: TierLocalNVMe, source: volumeSource{storageID: "5a-source"}, want: ""},
		{tier: TierLocalNVMe, source: volumeSource{snapshotID: "5b-source"}, want: ""},
	}
	for _, testCase := range cases {
		if got := placementServer(testCase.tier, testServerID, testCase.source); got != testCase.want {
			t.Fatalf("placementServer(%q, %+v) = %q, want %q", testCase.tier, testCase.source, got, testCase.want)
		}
	}
}

func localTopology(serverID string) *csi.TopologyRequirement {
	segments := map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: serverID}
	return &csi.TopologyRequirement{Requisite: []*csi.Topology{{Segments: segments}}, Preferred: []*csi.Topology{{Segments: segments}}}
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
		{http.StatusInternalServerError, codes.Unavailable},
	} {
		api := cloudfake.New()
		api.FailWith("CreateVolume", testCase.statusCode)
		csiDriver := newTestDriver(t, api, newFakeHost())
		_, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-error", gibibyte, nil))
		expectCode(t, createError, testCase.code)
	}
	api := cloudfake.New()
	api.FailWithError("CreateVolume", errNoAnswer)
	csiDriver := newTestDriver(t, api, newFakeHost())
	_, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-error", gibibyte, nil))
	expectCode(t, createError, codes.Unavailable)
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

// errNoAnswer is what a call that got no answer from the API returns: an error without an HTTP status.
var errNoAnswer = errors.New("read tcp 192.0.2.10:443: connection reset by peer")

// heldCallTimeout is the deadline of a call that is to run into an operation the fake never finishes. Such a call polls
// twice at most before it gets there, so the deadline is this long only to hold on a busy machine.
const heldCallTimeout = 250 * time.Millisecond

func heldCall(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), heldCallTimeout)
	t.Cleanup(cancel)
	return ctx
}

func storageNamed(name string, sequence int, state string) cloud.Volume {
	return cloud.Volume{ID: fmt.Sprintf("5a000000-0000-4000-8000-%012d", sequence), Zone: testZone, Title: name,
		Tier: cloudfake.DefaultStorageTier, State: state, SizeGibibytes: 1}
}

// expectStorages fails unless exactly count storages carry the volume's name, and returns them.
func expectStorages(t *testing.T, api *cloudfake.API, name string, count int) []cloud.Volume {
	t.Helper()
	volumes, listError := api.ListVolumes(context.Background())
	if listError != nil {
		t.Fatalf("ListVolumes: %v", listError)
	}
	var named []cloud.Volume
	for _, volume := range volumes {
		if volume.Title == name {
			named = append(named, volume)
		}
	}
	if len(named) != count {
		t.Fatalf("%d storages are named %s, want %d: %+v", len(named), name, count, named)
	}
	return named
}

// A create whose operation fails leaves its storage in `error`, and no PersistentVolume was made for it, so nothing
// but this call ever deletes it.
func TestACreateWhoseOperationFailsLeavesNoStorageBehindAndIsInternalOnlyOnceTheStorageIsGone(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	api.SettleCreates(2, cloud.StorageStateError)
	api.SettleDeletes(3, cloud.StorageStateDeleted)
	_, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-failing", gibibyte, nil))
	expectCode(t, createError, codes.Internal)
	expectStorages(t, api, "pvc-failing", 0)
	if api.Calls("CreateVolume") != 1 || api.Calls("DeleteVolume") != 1 {
		t.Fatalf("%d creates and %d deletes, want one of each", api.Calls("CreateVolume"), api.Calls("DeleteVolume"))
	}

	api.SettleCreates(0, cloud.StorageStateError)
	api.SettleDeletes(cloudfake.Never, cloud.StorageStateDeleted)
	_, heldError := csiDriver.CreateVolume(heldCall(t), createRequest("pvc-failing-slowly", gibibyte, nil))
	expectCode(t, heldError, codes.DeadlineExceeded)
	if left := expectStorages(t, api, "pvc-failing-slowly", 1); left[0].State != cloud.StorageStateDeleting {
		t.Fatalf("the failed storage is %s, want it being deleted", left[0].State)
	}
}

func TestAStorageThatTurnsToErrorWhileARetryWaitsForItIsDeletedAndTheCallIsNotFinalUntilItIsGone(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	turning := storageNamed("pvc-turning", 1, cloud.StorageStateCreating)
	api.AddVolume(turning)
	api.SettleVolume(turning.ID, 3, cloud.StorageStateError)
	_, turnedError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-turning", gibibyte, nil))
	expectCode(t, turnedError, codes.Internal)
	expectStorages(t, api, "pvc-turning", 0)

	held := storageNamed("pvc-turning-slowly", 2, cloud.StorageStateCreating)
	api.AddVolume(held)
	api.SettleVolume(held.ID, 1, cloud.StorageStateError)
	api.SettleDeletes(cloudfake.Never, cloud.StorageStateDeleted)
	_, heldError := csiDriver.CreateVolume(heldCall(t), createRequest("pvc-turning-slowly", gibibyte, nil))
	expectCode(t, heldError, codes.DeadlineExceeded)
	if left := expectStorages(t, api, "pvc-turning-slowly", 1); left[0].State != cloud.StorageStateDeleting {
		t.Fatalf("the failed storage is %s, want it being deleted", left[0].State)
	}
	api.SettleVolume(held.ID, 2, cloud.StorageStateDeleted)
	_, goneError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-turning-slowly", gibibyte, nil))
	expectCode(t, goneError, codes.Internal)
	expectStorages(t, api, "pvc-turning-slowly", 0)
	if api.Calls("CreateVolume") != 0 || api.Calls("DeleteVolume") != 2 {
		t.Fatalf("%d creates and %d deletes, want none and one for each storage", api.Calls("CreateVolume"), api.Calls("DeleteVolume"))
	}
}

// The driver looked past a storage that was `deleting` and created another of the same name beside it.
func TestARetryWhileTheFailedStorageIsStillBeingDeletedCreatesNoSecondStorage(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	failed := storageNamed("pvc-retried", 1, cloud.StorageStateError)
	api.AddVolume(failed)
	api.SettleDeletes(cloudfake.Never, cloud.StorageStateDeleted)
	for attempt := 1; attempt <= 2; attempt++ {
		_, heldError := csiDriver.CreateVolume(heldCall(t), createRequest("pvc-retried", gibibyte, nil))
		expectCode(t, heldError, codes.DeadlineExceeded)
		if left := expectStorages(t, api, "pvc-retried", 1); left[0].ID != failed.ID || left[0].State != cloud.StorageStateDeleting {
			t.Fatalf("attempt %d left %+v, want only the failed storage, being deleted", attempt, left)
		}
	}
	if api.Calls("CreateVolume") != 0 || api.Calls("DeleteVolume") != 1 {
		t.Fatalf("%d creates and %d deletes, want no create and one delete", api.Calls("CreateVolume"), api.Calls("DeleteVolume"))
	}

	api.SettleVolume(failed.ID, 2, cloud.StorageStateDeleted)
	_, goneError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-retried", gibibyte, nil))
	expectCode(t, goneError, codes.Internal)
	expectStorages(t, api, "pvc-retried", 0)
	if api.Calls("CreateVolume") != 0 {
		t.Fatalf("the call that saw the failed storage go created %d storages", api.Calls("CreateVolume"))
	}
	api.SettleDeletes(0, "")
	response, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-retried", gibibyte, nil))
	if createError != nil {
		t.Fatalf("CreateVolume once nothing is left: %v", createError)
	}
	if created := expectStorages(t, api, "pvc-retried", 1); created[0].ID != response.GetVolume().GetVolumeId() {
		t.Fatalf("returned %s, the storage is %s", response.GetVolume().GetVolumeId(), created[0].ID)
	}
}

// The API puts a storage whose delete failed back to `error`. Nothing looked at it again.
func TestADeleteThatFallsBackToErrorIsCleanedUpByTheNextCreateVolume(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	failed := storageNamed("pvc-fallback", 1, cloud.StorageStateError)
	api.AddVolume(failed)
	api.SettleDeletes(2, cloud.StorageStateError)
	_, fallbackError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-fallback", gibibyte, nil))
	expectCode(t, fallbackError, codes.Aborted)
	if left := expectStorages(t, api, "pvc-fallback", 1); left[0].ID != failed.ID || left[0].State != cloud.StorageStateError {
		t.Fatalf("the failed delete left %+v, want the storage back in error", left)
	}
	api.SettleDeletes(2, cloud.StorageStateDeleted)
	_, cleanedError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-fallback", gibibyte, nil))
	expectCode(t, cleanedError, codes.Internal)
	expectStorages(t, api, "pvc-fallback", 0)

	late := storageNamed("pvc-fallback-late", 2, cloud.StorageStateError)
	api.AddVolume(late)
	api.SettleDeletes(cloudfake.Never, cloud.StorageStateDeleted)
	_, heldError := csiDriver.CreateVolume(heldCall(t), createRequest("pvc-fallback-late", gibibyte, nil))
	expectCode(t, heldError, codes.DeadlineExceeded)
	expectStorages(t, api, "pvc-fallback-late", 1)
	api.SettleVolume(late.ID, 2, cloud.StorageStateError)
	api.SettleDeletes(1, cloud.StorageStateDeleted)
	_, lateError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-fallback-late", gibibyte, nil))
	expectCode(t, lateError, codes.Internal)
	expectStorages(t, api, "pvc-fallback-late", 0)
	if api.Calls("CreateVolume") != 0 || api.Calls("DeleteVolume") != 4 {
		t.Fatalf("%d creates and %d deletes, want no create and two deletes for each storage", api.Calls("CreateVolume"), api.Calls("DeleteVolume"))
	}
}

// A read that fails says nothing about the storage: the create goes on in the API, and the storage comes online with
// nobody left to own it if the answer ends provisioning.
func TestAnAPIReadThatFailsDoesNotEndProvisioningAndALaterCallAdoptsTheStorage(t *testing.T) {
	failures := []struct {
		name string
		fail func(api *cloudfake.API, method string)
	}{
		{"gets no answer", func(api *cloudfake.API, method string) { api.FailWithError(method, errNoAnswer) }},
		{"is answered 500", func(api *cloudfake.API, method string) { api.FailWith(method, http.StatusInternalServerError) }},
	}
	for _, method := range []string{"ListVolumes", "GetVolume", "GetOperation"} {
		for _, failure := range failures {
			t.Run(method+" "+failure.name, func(t *testing.T) {
				api := cloudfake.New()
				csiDriver := newTestDriver(t, api, newFakeHost())
				creates := 0
				if method == "GetOperation" {
					api.SettleCreates(cloudfake.Never, cloud.StorageStateOnline)
					creates = 1
				} else {
					api.AddVolume(storageNamed("pvc-unread", 1, cloud.StorageStateCreating))
				}
				failure.fail(api, method)
				_, failedError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-unread", gibibyte, nil))
				expectCode(t, failedError, codes.Unavailable)
				api.FailWith(method, 0)
				pending := expectStorages(t, api, "pvc-unread", 1)[0]

				api.SettleVolume(pending.ID, 0, cloud.StorageStateOnline)
				response, adoptError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-unread", gibibyte, nil))
				if adoptError != nil {
					t.Fatalf("CreateVolume once the storage is online: %v", adoptError)
				}
				expectStorages(t, api, "pvc-unread", 1)
				if response.GetVolume().GetVolumeId() != pending.ID || api.Calls("CreateVolume") != creates {
					t.Fatalf("returned %s after %d creates, want the storage %s after %d", response.GetVolume().GetVolumeId(),
						api.Calls("CreateVolume"), pending.ID, creates)
				}
			})
		}
	}
}

func TestCreateVolumeUsesTheOldestOfSeveralStoragesAndDeletesAFailedOne(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	createdAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	newer := storageNamed("pvc-twins", 1, cloud.StorageStateOnline)
	newer.CreatedAt = createdAt.Add(time.Hour)
	older := storageNamed("pvc-twins", 2, cloud.StorageStateOnline)
	older.CreatedAt = createdAt
	failed := storageNamed("pvc-twins", 3, cloud.StorageStateError)
	for _, volume := range []cloud.Volume{newer, older, failed} {
		api.AddVolume(volume)
	}
	response, createError := csiDriver.CreateVolume(context.Background(), createRequest("pvc-twins", gibibyte, nil))
	if createError != nil {
		t.Fatalf("CreateVolume: %v", createError)
	}
	if response.GetVolume().GetVolumeId() != older.ID {
		t.Fatalf("returned %s, want the oldest storage %s", response.GetVolume().GetVolumeId(), older.ID)
	}
	for _, left := range expectStorages(t, api, "pvc-twins", 2) {
		if left.ID == failed.ID {
			t.Fatalf("the failed storage %s was left beside the volume", failed.ID)
		}
	}
}

// The source was read before the lookup, so a retry whose source had gone answered NotFound, which ends provisioning
// with the storage of the first call left.
func TestARetryReturnsItsStorageAlthoughItsSourceIsGone(t *testing.T) {
	api := cloudfake.New()
	api.AddSnapshot(cloud.Snapshot{ID: "5b000000-0000-4000-8000-000000000001", StorageID: "gone", Zone: testZone, Title: "snap",
		State: cloud.StorageStateOnline, SizeGibibytes: 2})
	csiDriver := newTestDriver(t, api, newFakeHost())
	request := createRequest("pvc-restored-twice", gibibyte, nil)
	request.VolumeContentSource = &csi.VolumeContentSource{Type: &csi.VolumeContentSource_Snapshot{
		Snapshot: &csi.VolumeContentSource_SnapshotSource{SnapshotId: "5b000000-0000-4000-8000-000000000001"}}}
	first, firstError := csiDriver.CreateVolume(context.Background(), request)
	if firstError != nil {
		t.Fatalf("CreateVolume: %v", firstError)
	}
	if _, deleteError := api.DeleteSnapshot(context.Background(), "5b000000-0000-4000-8000-000000000001"); deleteError != nil {
		t.Fatalf("DeleteSnapshot: %v", deleteError)
	}
	again, againError := csiDriver.CreateVolume(context.Background(), request)
	if againError != nil || again.GetVolume().GetVolumeId() != first.GetVolume().GetVolumeId() {
		t.Fatalf("the retry: %v, %v; want the storage %s", again, againError, first.GetVolume().GetVolumeId())
	}
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

// The API refuses to delete a storage it is already deleting. Answered as FailedPrecondition, that failed every retry
// of a DeleteVolume whose first call had timed out.
func TestDeleteVolumeOfAStorageAlreadyBeingDeletedSucceedsOnceItIsGone(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	deleting := storageNamed("pvc-deleting", 1, cloud.StorageStateDeleting)
	api.AddVolume(deleting)
	api.SettleVolume(deleting.ID, 3, cloud.StorageStateDeleted)
	if _, deleteError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: deleting.ID}); deleteError != nil {
		t.Fatalf("DeleteVolume of a storage being deleted: %v", deleteError)
	}
	expectStorages(t, api, "pvc-deleting", 0)
	if api.Calls("DeleteVolume") != 0 {
		t.Fatalf("%d deletes were sent for a storage the API was already deleting", api.Calls("DeleteVolume"))
	}

	held := storageNamed("pvc-deleting-slowly", 2, cloud.StorageStateDeleting)
	api.AddVolume(held)
	_, heldError := csiDriver.DeleteVolume(heldCall(t), &csi.DeleteVolumeRequest{VolumeId: held.ID})
	expectCode(t, heldError, codes.DeadlineExceeded)

	raced := storageNamed("pvc-deleting-raced", 3, cloud.StorageStateDeleting)
	api.AddVolume(raced)
	api.SettleVolume(raced.ID, 3, cloud.StorageStateDeleted)
	stale := raced
	stale.State = cloud.StorageStateOnline
	csiDriver.options.API = &staleReadAPI{API: api, stale: &stale}
	if _, racedError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: raced.ID}); racedError != nil {
		t.Fatalf("DeleteVolume refused with 409 for a storage being deleted: %v", racedError)
	}
	expectStorages(t, api, "pvc-deleting-raced", 0)
	if api.Calls("DeleteVolume") != 1 {
		t.Fatalf("%d deletes, want the one the API refused", api.Calls("DeleteVolume"))
	}
	csiDriver.options.API = api

	failed := storageNamed("pvc-deleting-failed", 4, cloud.StorageStateError)
	api.AddVolume(failed)
	if _, failedError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: failed.ID}); failedError != nil {
		t.Fatalf("DeleteVolume of a storage in error: %v", failedError)
	}
	expectStorages(t, api, "pvc-deleting-failed", 0)

	online := storageNamed("pvc-deleting-refused", 5, cloud.StorageStateOnline)
	api.AddVolume(online)
	api.FailWithDetail("DeleteVolume", http.StatusConflict, "the storage has snapshots; delete them first")
	_, refusedError := csiDriver.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{VolumeId: online.ID})
	expectCode(t, refusedError, codes.FailedPrecondition)
}

// staleReadAPI answers the first read of a storage with what the storage was before its delete began, the way a
// DeleteVolume sees a storage whose delete starts between its read and its own delete.
type staleReadAPI struct {
	*cloudfake.API
	stale *cloud.Volume
}

func (api *staleReadAPI) GetVolume(ctx context.Context, volumeID string) (cloud.Volume, error) {
	if api.stale != nil && api.stale.ID == volumeID {
		stale := *api.stale
		api.stale = nil
		return stale, nil
	}
	return api.API.GetVolume(ctx, volumeID)
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

// external-provisioner hands a claim back to the scheduler, to pick another node, only for ResourceExhausted. Any other
// code makes it retry the node the claim was bound to, for as long as the pod waits.
func TestCreateVolumeOnANodeWithoutRoomAsksForAnotherNode(t *testing.T) {
	request := createRequest("pvc-full-node", gibibyte, map[string]string{ParameterTier: TierLocalNVMe})
	request.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: testServerID}}},
		Preferred: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: testServerID}}},
	}
	cases := []struct {
		name string
		fail func(api *cloudfake.API)
		code codes.Code
	}{
		{"the node's pool or the tier takes no new storage", func(api *cloudfake.API) {
			api.FailWithShortfall("CreateVolume", http.StatusConflict, cloud.ShortfallReasonNotOffered)
		}, codes.ResourceExhausted},
		{"the zone has no room right now", func(api *cloudfake.API) {
			api.FailWithShortfall("CreateVolume", http.StatusServiceUnavailable, cloud.ShortfallReasonCapacity)
		}, codes.ResourceExhausted},
		{"the wording of an API without a reason member", func(api *cloudfake.API) {
			api.FailWithDetail("CreateVolume", http.StatusConflict,
				"the storage backend is not available in this zone: the node's Ankra Local pool takes no new volumes: 60 GiB more would pass 80 %")
		}, codes.ResourceExhausted},
		{"today's wording without a reason member is not matched by its text", func(api *cloudfake.API) {
			api.FailWithDetail("CreateVolume", http.StatusConflict, cloud.StorageShortfallDetail)
		}, codes.FailedPrecondition},
		{"another conflict", func(api *cloudfake.API) {
			api.FailWithDetail("CreateVolume", http.StatusConflict, "the storage is attached")
		}, codes.FailedPrecondition},
		{"the API is unavailable for another reason", func(api *cloudfake.API) {
			api.FailWith("CreateVolume", http.StatusServiceUnavailable)
		}, codes.Unavailable},
		{"a reason the driver does not know", func(api *cloudfake.API) {
			api.FailWithShortfall("CreateVolume", http.StatusConflict, "something_else")
		}, codes.FailedPrecondition},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			api := cloudfake.New()
			csiDriver := newTestDriver(t, api, newFakeHost())
			testCase.fail(api)
			_, createError := csiDriver.CreateVolume(context.Background(), request)
			expectCode(t, createError, testCase.code)
		})
	}
}

// The fake refuses a storage that passes the room of its node's pool the way the API does, so this is the path a full
// node takes from the API's answer to the code external-provisioner acts on.
func TestCreateVolumePastTheRoomOfItsNodeAsksForAnotherNode(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	api.PlaceServer(testServerID, testComputeNode)
	api.SetLocalRoom(testComputeNode, 5)
	request := createRequest("pvc-past-the-room", 10*gibibyte, map[string]string{ParameterTier: TierLocalNVMe})
	request.AccessibilityRequirements = &csi.TopologyRequirement{
		Requisite: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: testServerID}}},
		Preferred: []*csi.Topology{{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: testServerID}}},
	}
	_, createError := csiDriver.CreateVolume(context.Background(), request)
	expectCode(t, createError, codes.ResourceExhausted)
	if api.VolumeCount() != 0 {
		t.Fatalf("a refused storage was created: %d storages", api.VolumeCount())
	}
}
