package reclaimspace

import (
	"context"
	"slices"
	"testing"

	"github.com/csi-addons/spec/lib/go/identity"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func capStrings(caps []*identity.Capability) []string {
	var out []string
	for _, c := range caps {
		switch t := c.GetType().(type) {
		case *identity.Capability_Service_:
			out = append(out, "service:"+t.Service.GetType().String())
		case *identity.Capability_ReclaimSpace_:
			out = append(out, "reclaimspace:"+t.ReclaimSpace.GetType().String())
		}
	}
	return out
}

func TestGetCapabilities(t *testing.T) {
	tests := []struct {
		mode Mode
		want []string
	}{
		{ModeController, []string{"service:CONTROLLER_SERVICE", "reclaimspace:OFFLINE"}},
		{ModeNode, []string{"service:NODE_SERVICE", "reclaimspace:ONLINE"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			s := &identityServer{name: "cinder.csi.openstack.org", version: "1.0.0.0", mode: tt.mode}
			resp, err := s.GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if got := capStrings(resp.GetCapabilities()); !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetIdentity(t *testing.T) {
	s := &identityServer{name: "cinder.csi.openstack.org", version: "1.0.0.0"}
	resp, err := s.GetIdentity(context.Background(), &identity.GetIdentityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetName() != "cinder.csi.openstack.org" || resp.GetVendorVersion() != "1.0.0.0" {
		t.Errorf("got %q %q", resp.GetName(), resp.GetVendorVersion())
	}
	_, err = (&identityServer{}).GetIdentity(context.Background(), &identity.GetIdentityRequest{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("unconfigured identity: %v, want Unavailable", err)
	}
}

func TestProbe(t *testing.T) {
	resp, err := (&identityServer{}).Probe(context.Background(), &identity.ProbeRequest{})
	if err != nil || !resp.GetReady().GetValue() {
		t.Errorf("probe: %v ready=%v", err, resp.GetReady().GetValue())
	}
}
