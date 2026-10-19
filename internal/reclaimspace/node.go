package reclaimspace

import (
	"context"
	"errors"
	"io/fs"
	"log"
	"sync"

	"github.com/csi-addons/spec/lib/go/reclaimspace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type nodeServer struct {
	reclaimspace.UnimplementedReclaimSpaceNodeServer
	trim func(path string) (uint64, error)
	// inFlight holds the IDs of volumes being trimmed.
	inFlight sync.Map
}

func (s *nodeServer) NodeReclaimSpace(_ context.Context, req *reclaimspace.NodeReclaimSpaceRequest) (*reclaimspace.NodeReclaimSpaceResponse, error) {
	id := req.GetVolumeId()
	if id == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID missing in request")
	}
	if req.GetVolumeCapability().GetBlock() != nil {
		return nil, status.Error(codes.Unimplemented, "reclaim space is not supported for block volumes")
	}
	path := req.GetStagingTargetPath()
	if path == "" {
		path = req.GetVolumePath()
	}
	if path == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path and volume path missing in request")
	}

	// FITRIM outlives the caller's per-request timeout, so the caller's
	// retries would otherwise overlap on the backend.
	if _, busy := s.inFlight.LoadOrStore(id, struct{}{}); busy {
		return nil, status.Errorf(codes.Aborted, "reclaim space already in progress for volume %s", id)
	}
	defer s.inFlight.Delete(id)

	trimmed, err := s.trim(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, status.Errorf(codes.NotFound, "path %s does not exist", path)
	case errors.Is(err, errNotMountPoint):
		// Trimming it would discard free blocks of the node's own filesystem.
		return nil, status.Errorf(codes.FailedPrecondition, "path %s is not a mount point", path)
	case err != nil:
		return nil, status.Errorf(codes.Internal, "FITRIM on %s failed: %v", path, err)
	}
	log.Printf("volume %s: %d bytes trimmed on %s", id, trimmed, path)
	return &reclaimspace.NodeReclaimSpaceResponse{}, nil
}
