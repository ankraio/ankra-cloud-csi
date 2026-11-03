package driver

import (
	"context"
	"sync"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/ankraio/ankra-cloud-csi/internal/cloud"
)

// DefaultCapacityCacheTTL is how long one read of a server's storage capacity answers GetCapacity. external-provisioner
// asks once per StorageClass and node on every poll, so within one poll the classes of a node share a single API call,
// and the next poll reads the API again.
const DefaultCapacityCacheTTL = 10 * time.Second

// GetCapacity answers how much new storage of a StorageClass's tier the node in the request's topology can still take.
// With CSIDriver storageCapacity and external-provisioner's --enable-capacity, it is published per node as a
// CSIStorageCapacity, and the scheduler binds a WaitForFirstConsumer claim only to a node whose capacity fits it.
//
// The answer is the API's own (get_server_storage_capacity), computed by the rule CreateVolume is refused with:
//   - a tier the API limits per node (local-nvme: the Ankra Local pool of the compute node that runs the server) reports
//     the pool's remaining room as the available capacity and the largest volume CreateVolume takes there as the
//     maximum volume size, 0 for a full pool;
//   - a tier the API does not limit per node (the Ceph tiers, a pool the node's agent has not reported, a tier the zone
//     refuses everywhere) reports the largest volume the API takes at all, which is what the scheduler assumed before
//     capacity tracking: no node choice helps a claim the whole zone refuses, and CreateVolume then says why;
//   - a server the API does not know (404, also an API from before the endpoint) is answered the same way, so a stale
//     node or an older API never blocks scheduling.
//
// Any other API failure is an error: external-provisioner keeps the CSIStorageCapacity it published last and asks again.
func (driver *Driver) GetCapacity(ctx context.Context, request *csi.GetCapacityRequest) (*csi.GetCapacityResponse, error) {
	tier := request.GetParameters()[ParameterTier]
	if len(request.GetVolumeCapabilities()) > 0 && validateCapabilities(request.GetVolumeCapabilities()) != nil {
		// No volume with these capabilities can be created anywhere.
		return capacityResponse(0, 0), nil
	}
	server := request.GetAccessibleTopology().GetSegments()[TopologyNodeKey]
	if server == "" {
		// Not a node's segment. CreateVolume refuses local-nvme without the selected node, and takes any other tier
		// wherever the zone has room for it.
		if tier == TierLocalNVMe {
			return capacityResponse(0, 0), nil
		}
		return unlimitedCapacity(), nil
	}
	capacity, capacityError := driver.storageCapacity(ctx, server)
	if cloud.IsNotFound(capacityError) {
		driver.logger.Warn("the API publishes no storage capacity for the server; reporting the largest volume it takes",
			"server", server, "error", capacityError)
		return unlimitedCapacity(), nil
	}
	if capacityError != nil {
		return nil, apiStatus(capacityError, "read the storage capacity next to server %s", server)
	}
	if tier == "" {
		tier = capacity.DefaultTier
	}
	tierCapacity, isKnown := capacity.Tier(tier)
	if !isKnown || tierCapacity.AvailableGibibytes == nil {
		return unlimitedCapacity(), nil
	}
	driver.noteRefusal(server, tier, tierCapacity.Refusal)
	return capacityResponse(max(*tierCapacity.AvailableGibibytes, 0)*gibibyte, max(tierCapacity.MaximumGibibytes, 0)*gibibyte), nil
}

func capacityResponse(availableBytes int64, maximumVolumeBytes int64) *csi.GetCapacityResponse {
	return &csi.GetCapacityResponse{AvailableCapacity: availableBytes, MaximumVolumeSize: wrapperspb.Int64(maximumVolumeBytes)}
}

// unlimitedCapacity is the largest volume the API takes, where it sets no per-node limit.
func unlimitedCapacity() *csi.GetCapacityResponse {
	return capacityResponse(maximumVolumeGibibytes*gibibyte, maximumVolumeGibibytes*gibibyte)
}

// capacityCache holds the last read of each server's storage capacity, and the refusal last logged for each server and
// tier so a full node is logged when it fills or frees up, not on every poll.
type capacityCache struct {
	mutex    sync.Mutex
	entries  map[string]capacityEntry
	refusals map[string]string
}

type capacityEntry struct {
	capacity cloud.StorageCapacity
	readAt   time.Time
}

// storageCapacity reads the server's storage capacity from the API, or from a read younger than the cache TTL.
func (driver *Driver) storageCapacity(ctx context.Context, server string) (cloud.StorageCapacity, error) {
	ttl := driver.options.CapacityCacheTTL
	driver.capacities.mutex.Lock()
	entry, isCached := driver.capacities.entries[server]
	driver.capacities.mutex.Unlock()
	if isCached && ttl > 0 && driver.now().Sub(entry.readAt) < ttl {
		return entry.capacity, nil
	}
	capacity, readError := driver.options.API.GetStorageCapacity(ctx, server)
	if readError != nil {
		return cloud.StorageCapacity{}, readError
	}
	driver.capacities.mutex.Lock()
	driver.capacities.entries[server] = capacityEntry{capacity: capacity, readAt: driver.now()}
	driver.capacities.mutex.Unlock()
	return capacity, nil
}

// forgetCapacities drops every cached read: a volume was created, grown or deleted, so a pool's room changed.
func (driver *Driver) forgetCapacities() {
	driver.capacities.mutex.Lock()
	defer driver.capacities.mutex.Unlock()
	clear(driver.capacities.entries)
}

// noteRefusal logs when a server's tier stops or starts taking new volumes.
func (driver *Driver) noteRefusal(server string, tier string, refusal string) {
	key := server + "/" + tier
	driver.capacities.mutex.Lock()
	previous := driver.capacities.refusals[key]
	if refusal == "" {
		delete(driver.capacities.refusals, key)
	} else {
		driver.capacities.refusals[key] = refusal
	}
	driver.capacities.mutex.Unlock()
	switch {
	case refusal != "" && refusal != previous:
		driver.logger.Info("the node takes no new volumes of the tier", "server", server, "tier", tier, "reason", refusal)
	case refusal == "" && previous != "":
		driver.logger.Info("the node takes new volumes of the tier again", "server", server, "tier", tier)
	}
}
