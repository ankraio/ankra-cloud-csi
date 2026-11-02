package driver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud/cloudfake"
)

const testComputeNode = "compute-node-1"

// nodeSegment is the topology segment external-provisioner asks GetCapacity about: the one NodeGetInfo reports.
func nodeSegment() *csi.Topology {
	return &csi.Topology{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: testServerID}}
}

func capacityRequest(tier string, topology *csi.Topology) *csi.GetCapacityRequest {
	request := &csi.GetCapacityRequest{AccessibleTopology: topology, Parameters: map[string]string{}}
	if tier != "" {
		request.Parameters[ParameterTier] = tier
	}
	return request
}

func expectCapacity(t *testing.T, response *csi.GetCapacityResponse, callError error, availableBytes int64, maximumVolumeBytes int64) {
	t.Helper()
	if callError != nil {
		t.Fatalf("GetCapacity: %v", callError)
	}
	if response.GetAvailableCapacity() != availableBytes || response.GetMaximumVolumeSize() == nil ||
		response.GetMaximumVolumeSize().GetValue() != maximumVolumeBytes {
		t.Fatalf("capacity %d, maximum volume %v; want %d and %d", response.GetAvailableCapacity(), response.GetMaximumVolumeSize(),
			availableBytes, maximumVolumeBytes)
	}
}

func localCreateRequest(name string, gibibytes int64) *csi.CreateVolumeRequest {
	request := createRequest(name, gibibytes*gibibyte, map[string]string{ParameterTier: TierLocalNVMe})
	request.AccessibilityRequirements = &csi.TopologyRequirement{Requisite: []*csi.Topology{nodeSegment()}, Preferred: []*csi.Topology{nodeSegment()}}
	return request
}

func TestTheControllerAdvertisesGetCapacity(t *testing.T) {
	csiDriver := newTestDriver(t, cloudfake.New(), newFakeHost())
	response, _ := csiDriver.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	for _, capability := range response.GetCapabilities() {
		if capability.GetRpc().GetType() == csi.ControllerServiceCapability_RPC_GET_CAPACITY {
			return
		}
	}
	t.Fatal("GET_CAPACITY is not advertised, so external-provisioner publishes no capacity")
}

// TestANodesCapacityIsWhatCreateVolumeTakesThere is the bug this fixes: a node whose Ankra Local pool is full must
// publish no room, so the scheduler binds the claim elsewhere instead of to a node where CreateVolume answers 409.
func TestANodesCapacityIsWhatCreateVolumeTakesThere(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	api.PlaceServer(testServerID, testComputeNode)
	api.SetLocalRoom(testComputeNode, 100)
	ctx := context.Background()

	response, capacityError := csiDriver.GetCapacity(ctx, capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCapacity(t, response, capacityError, 100*gibibyte, 100*gibibyte)

	if _, createError := csiDriver.CreateVolume(ctx, localCreateRequest("pvc-fits", 96)); createError != nil {
		t.Fatalf("a volume within the published room is created: %v", createError)
	}
	response, capacityError = csiDriver.GetCapacity(ctx, capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCapacity(t, response, capacityError, 4*gibibyte, 4*gibibyte)

	if _, createError := csiDriver.CreateVolume(ctx, localCreateRequest("pvc-too-large", 5)); createError == nil {
		t.Fatal("a volume past the published room is refused by the API too")
	}
	created, createError := csiDriver.CreateVolume(ctx, localCreateRequest("pvc-last", 4))
	if createError != nil {
		t.Fatalf("the last 4 GiB fit: %v", createError)
	}
	response, capacityError = csiDriver.GetCapacity(ctx, capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCapacity(t, response, capacityError, 0, 0)

	if _, deleteError := csiDriver.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: created.GetVolume().GetVolumeId()}); deleteError != nil {
		t.Fatal(deleteError)
	}
	response, capacityError = csiDriver.GetCapacity(ctx, capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCapacity(t, response, capacityError, 4*gibibyte, 4*gibibyte)
}

func TestAClassWithoutATierHasTheZonesDefaultTiersCapacity(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	api.PlaceServer(testServerID, testComputeNode)
	api.SetLocalRoom(testComputeNode, 0)
	ctx := context.Background()

	response, capacityError := csiDriver.GetCapacity(ctx, capacityRequest("", nodeSegment()))
	expectCapacity(t, response, capacityError, maximumVolumeGibibytes*gibibyte, maximumVolumeGibibytes*gibibyte)

	api.SetZoneDefaultTier(testZone, cloudfake.LocalStorageTier)
	csiDriver.forgetCapacities()
	response, capacityError = csiDriver.GetCapacity(ctx, capacityRequest("", nodeSegment()))
	expectCapacity(t, response, capacityError, 0, 0)
}

func TestTiersTheAPIDoesNotLimitPerNodeTakeTheLargestVolume(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	api.PlaceServer(testServerID, testComputeNode)
	api.SetLocalRoom(testComputeNode, 0)
	ctx := context.Background()
	largest := int64(maximumVolumeGibibytes) * gibibyte

	response, capacityError := csiDriver.GetCapacity(ctx, capacityRequest(cloudfake.DefaultStorageTier, nodeSegment()))
	expectCapacity(t, response, capacityError, largest, largest)

	// A tier the zone refuses everywhere is no node's doing: the scheduler is not asked to choose, and CreateVolume
	// says why.
	api.RefuseTier("hdd", "storage tier hdd is not offered in zone fsn1")
	response, capacityError = csiDriver.GetCapacity(ctx, capacityRequest("hdd", nodeSegment()))
	expectCapacity(t, response, capacityError, largest, largest)

	response, capacityError = csiDriver.GetCapacity(ctx, capacityRequest("a-tier-the-api-does-not-list", nodeSegment()))
	expectCapacity(t, response, capacityError, largest, largest)

	// A node whose agent reports no pool is not gated by the API either.
	api.PlaceServer(testServerID, "a-node-without-a-reported-pool")
	csiDriver.forgetCapacities()
	response, capacityError = csiDriver.GetCapacity(ctx, capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCapacity(t, response, capacityError, largest, largest)
}

func TestARequestForNoNodeInParticular(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	ctx := context.Background()
	zoneOnly := &csi.Topology{Segments: map[string]string{TopologyZoneKey: testZone}}

	response, capacityError := csiDriver.GetCapacity(ctx, capacityRequest(TierLocalNVMe, zoneOnly))
	expectCapacity(t, response, capacityError, 0, 0)
	response, capacityError = csiDriver.GetCapacity(ctx, &csi.GetCapacityRequest{})
	largest := int64(maximumVolumeGibibytes) * gibibyte
	expectCapacity(t, response, capacityError, largest, largest)
	if api.Calls("GetStorageCapacity") != 0 {
		t.Fatal("a request without a node has nothing to ask the API")
	}

	unsupported := &csi.GetCapacityRequest{AccessibleTopology: nodeSegment(), VolumeCapabilities: []*csi.VolumeCapability{{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}}}
	response, capacityError = csiDriver.GetCapacity(ctx, unsupported)
	expectCapacity(t, response, capacityError, 0, 0)
}

func TestAServerTheAPIDoesNotKnowNeverBlocksScheduling(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	gone := &csi.Topology{Segments: map[string]string{TopologyZoneKey: testZone, TopologyNodeKey: "01a0d058-e588-7fff-8000-00000000dead"}}
	response, capacityError := csiDriver.GetCapacity(context.Background(), capacityRequest(TierLocalNVMe, gone))
	largest := int64(maximumVolumeGibibytes) * gibibyte
	expectCapacity(t, response, capacityError, largest, largest)

	// An API without the endpoint answers 404 for every server, and is treated the same way.
	api.FailWith("GetStorageCapacity", http.StatusNotFound)
	response, capacityError = csiDriver.GetCapacity(context.Background(), capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCapacity(t, response, capacityError, largest, largest)
}

func TestAnAPIFailureKeepsTheLastPublishedCapacity(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	api.FailWith("GetStorageCapacity", http.StatusServiceUnavailable)
	_, capacityError := csiDriver.GetCapacity(context.Background(), capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCode(t, capacityError, codes.Unavailable)
}

func TestTheClassesOfANodeShareOneReadOfTheAPI(t *testing.T) {
	api := cloudfake.New()
	csiDriver := newTestDriver(t, api, newFakeHost())
	api.PlaceServer(testServerID, testComputeNode)
	api.SetLocalRoom(testComputeNode, 50)
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	csiDriver.now = func() time.Time { return now }
	ctx := context.Background()
	for _, tier := range []string{"", cloudfake.DefaultStorageTier, "hdd", TierLocalNVMe} {
		if _, capacityError := csiDriver.GetCapacity(ctx, capacityRequest(tier, nodeSegment())); capacityError != nil {
			t.Fatal(capacityError)
		}
	}
	if calls := api.Calls("GetStorageCapacity"); calls != 1 {
		t.Fatalf("one poll's classes of one node read the API once, not %d times", calls)
	}

	api.SetLocalRoom(testComputeNode, 20)
	now = now.Add(DefaultCapacityCacheTTL)
	response, capacityError := csiDriver.GetCapacity(ctx, capacityRequest(TierLocalNVMe, nodeSegment()))
	expectCapacity(t, response, capacityError, 20*gibibyte, 20*gibibyte)
	if calls := api.Calls("GetStorageCapacity"); calls != 2 {
		t.Fatalf("the next poll reads the API again: %d calls", calls)
	}
}
