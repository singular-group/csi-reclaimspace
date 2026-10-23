package reclaimspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/csi-addons/spec/lib/go/reclaimspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNodeReclaimSpace(t *testing.T) {
	blockCap := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}}}
	mountCap := &csi.VolumeCapability{AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{}}}
	staged := func() *reclaimspace.NodeReclaimSpaceRequest {
		return &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v", StagingTargetPath: "/stage"}
	}

	tests := []struct {
		name       string
		req        *reclaimspace.NodeReclaimSpaceRequest
		trimErr    error
		wantCode   codes.Code
		wantTrimOn string // "" = trim must not be called
	}{
		{"missing volume id", &reclaimspace.NodeReclaimSpaceRequest{StagingTargetPath: "/stage"}, nil, codes.InvalidArgument, ""},
		{"block volume", &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v", StagingTargetPath: "/stage", VolumeCapability: blockCap}, nil, codes.InvalidArgument, ""},
		{"no paths", &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v"}, nil, codes.InvalidArgument, ""},
		{"staging path wins", &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v", StagingTargetPath: "/stage", VolumePath: "/pub", VolumeCapability: mountCap}, nil, codes.OK, "/stage"},
		{"falls back to volume path", &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v", VolumePath: "/pub"}, nil, codes.OK, "/pub"},
		{"path missing", staged(), fmt.Errorf("open /stage: %w", fs.ErrNotExist), codes.NotFound, "/stage"},
		// Unmounted directory or a file: trimming it would hit the node's disk.
		{"not a mount point", staged(), errNotMountPoint, codes.FailedPrecondition, "/stage"},
		{"trim fails", staged(), errors.New("operation not supported"), codes.Internal, "/stage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var trimmedOn string
			s := &nodeServer{trim: func(path string) (uint64, error) {
				trimmedOn = path
				return 4096, tt.trimErr
			}}
			_, err := s.NodeReclaimSpace(context.Background(), tt.req)
			if got := status.Code(err); got != tt.wantCode {
				t.Errorf("code %v, want %v (err: %v)", got, tt.wantCode, err)
			}
			if trimmedOn != tt.wantTrimOn {
				t.Errorf("trimmed %q, want %q", trimmedOn, tt.wantTrimOn)
			}
		})
	}
}

func TestNodeReclaimSpaceInFlight(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var mu sync.Mutex
	trims := map[string]int{}
	s := &nodeServer{trim: func(path string) (uint64, error) {
		mu.Lock()
		trims[path]++
		n := trims[path]
		mu.Unlock()
		// Only the first trim of the first volume blocks, so a missing guard
		// fails the assertions below instead of hanging the test.
		if path == "/first" && n == 1 {
			close(entered)
			<-release
		}
		return 4096, nil
	}}
	reclaim := func(id, path string) error {
		_, err := s.NodeReclaimSpace(context.Background(), &reclaimspace.NodeReclaimSpaceRequest{VolumeId: id, StagingTargetPath: path})
		return err
	}

	errA := make(chan error, 1)
	go func() { errA <- reclaim("vol-1", "/first") }()
	<-entered

	if err := reclaim("vol-1", "/first"); status.Code(err) != codes.Aborted {
		t.Errorf("same volume while in flight: %v, want Aborted", err)
	}
	if err := reclaim("vol-2", "/second"); err != nil {
		t.Errorf("other volume while vol-1 in flight: %v", err)
	}
	close(release)
	if err := <-errA; err != nil {
		t.Errorf("first call: %v", err)
	}
	// The guard is released once the first call returns.
	if err := reclaim("vol-1", "/first"); err != nil {
		t.Errorf("same volume after the first call returned: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := map[string]int{"/first": 2, "/second": 1}; !maps.Equal(trims, want) {
		t.Errorf("trims %v, want %v", trims, want)
	}
}
