package cosi

import (
	"context"

	cosiapi "sigs.k8s.io/container-object-storage-interface/proto"
)

// DriverGetInfo names the driver; the provisioner sidecar serves the Buckets and BucketAccesses of this name.
func (driver *Driver) DriverGetInfo(context.Context, *cosiapi.DriverGetInfoRequest) (*cosiapi.DriverGetInfoResponse, error) {
	return &cosiapi.DriverGetInfoResponse{Name: Name}, nil
}
