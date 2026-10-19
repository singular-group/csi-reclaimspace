package reclaimspace

import (
	"net"
	"time"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/reclaimspace"
	"google.golang.org/grpc"
)

// Mode is the side of the CSI driver the sidecar runs next to.
type Mode string

const (
	ModeNode       Mode = "node"
	ModeController Mode = "controller"
)

// Options configures the server.
type Options struct {
	DriverName string
	Version    string
	Mode       Mode
}

// Server serves the CSI-Addons gRPC services.
type Server struct {
	grpc *grpc.Server
}

// NewServer registers Identity plus the ReclaimSpace service of opts.Mode.
func NewServer(opts Options) *Server {
	return newServer(opts, fitrim)
}

func newServer(opts Options, trim func(path string) (uint64, error)) *Server {
	s := grpc.NewServer()
	identity.RegisterIdentityServer(s, &identityServer{name: opts.DriverName, version: opts.Version, mode: opts.Mode})
	switch opts.Mode {
	case ModeController:
		reclaimspace.RegisterReclaimSpaceControllerServer(s, &controllerServer{})
	case ModeNode:
		reclaimspace.RegisterReclaimSpaceNodeServer(s, &nodeServer{trim: trim})
	}
	return &Server{grpc: s}
}

// Serve blocks until lis fails or a Shutdown has let every RPC finish.
func (s *Server) Serve(lis net.Listener) error {
	return s.grpc.Serve(lis)
}

// Shutdown stops accepting RPCs and waits up to timeout for running ones. A
// FITRIM still running then is left to die with the process: it leaves the
// filesystem consistent, and the pod has to stop within its grace period.
// grpc's Stop is not used: it deadlocks against a GracefulStop that is waiting
// for a handler.
func (s *Server) Shutdown(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.grpc.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// controllerServer only exists so the controller can advertise ReclaimSpace
// OFFLINE; there is no controller-side reclaim.
type controllerServer struct {
	reclaimspace.UnimplementedReclaimSpaceControllerServer
}
