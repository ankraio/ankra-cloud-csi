package driver

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// GetPluginInfo names the driver.
func (driver *Driver) GetPluginInfo(context.Context, *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{Name: Name, VendorVersion: driver.options.Version}, nil
}

// GetPluginCapabilities: a controller service, topology constraints and online expansion.
func (driver *Driver) GetPluginCapabilities(context.Context, *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	service := func(kind csi.PluginCapability_Service_Type) *csi.PluginCapability {
		return &csi.PluginCapability{Type: &csi.PluginCapability_Service_{Service: &csi.PluginCapability_Service{Type: kind}}}
	}
	return &csi.GetPluginCapabilitiesResponse{Capabilities: []*csi.PluginCapability{
		service(csi.PluginCapability_Service_CONTROLLER_SERVICE),
		service(csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS),
		{Type: &csi.PluginCapability_VolumeExpansion_{VolumeExpansion: &csi.PluginCapability_VolumeExpansion{
			Type: csi.PluginCapability_VolumeExpansion_ONLINE}}},
	}}, nil
}

// Probe answers ready: the driver holds no state that needs warming up.
func (driver *Driver) Probe(context.Context, *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	return &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}
