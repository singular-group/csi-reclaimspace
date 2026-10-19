// Package reclaimspace serves the CSI-Addons Identity and ReclaimSpace
// services (github.com/csi-addons/spec) next to a CSI driver.
package reclaimspace

import (
	"context"

	"github.com/csi-addons/spec/lib/go/identity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

type identityServer struct {
	identity.UnimplementedIdentityServer
	name    string
	version string
	mode    Mode
}

// GetIdentity names the CSI driver this sidecar serves: the CSI-Addons
// k8s-sidecar registers its CSIAddonsNode under this name.
func (s *identityServer) GetIdentity(_ context.Context, _ *identity.GetIdentityRequest) (*identity.GetIdentityResponse, error) {
	if s.name == "" || s.version == "" {
		return nil, status.Error(codes.Unavailable, "driver name or version not configured")
	}
	return &identity.GetIdentityResponse{Name: s.name, VendorVersion: s.version}, nil
}

// GetCapabilities advertises ReclaimSpace OFFLINE for the controller: the
// CSI-Addons controller runs the node-side ONLINE operation only when a
// leader advertises it.
func (s *identityServer) GetCapabilities(_ context.Context, _ *identity.GetCapabilitiesRequest) (*identity.GetCapabilitiesResponse, error) {
	var caps []*identity.Capability
	switch s.mode {
	case ModeController:
		caps = append(caps,
			serviceCap(identity.Capability_Service_CONTROLLER_SERVICE),
			reclaimSpaceCap(identity.Capability_ReclaimSpace_OFFLINE))
	case ModeNode:
		caps = append(caps,
			serviceCap(identity.Capability_Service_NODE_SERVICE),
			reclaimSpaceCap(identity.Capability_ReclaimSpace_ONLINE))
	}
	return &identity.GetCapabilitiesResponse{Capabilities: caps}, nil
}

func (s *identityServer) Probe(_ context.Context, _ *identity.ProbeRequest) (*identity.ProbeResponse, error) {
	return &identity.ProbeResponse{Ready: wrapperspb.Bool(true)}, nil
}

func serviceCap(t identity.Capability_Service_Type) *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_Service_{
		Service: &identity.Capability_Service{Type: t},
	}}
}

func reclaimSpaceCap(t identity.Capability_ReclaimSpace_Type) *identity.Capability {
	return &identity.Capability{Type: &identity.Capability_ReclaimSpace_{
		ReclaimSpace: &identity.Capability_ReclaimSpace{Type: t},
	}}
}
