package reclaimspace

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/reclaimspace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func startServer(t *testing.T, mode Mode) *grpc.ClientConn {
	t.Helper()
	// Not t.TempDir(): it embeds the test name and can exceed the unix
	// socket length limit.
	dir, err := os.MkdirTemp("", "crs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	lis, err := net.Listen("unix", filepath.Join(dir, "csi-addons.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(Options{DriverName: "cinder.csi.openstack.org", Version: "1.0.0.0", Mode: mode})
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(func() { s.Shutdown(time.Second) })
	conn, err := grpc.NewClient("unix://"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestServerNodeMode(t *testing.T) {
	conn := startServer(t, ModeNode)
	ctx := context.Background()

	caps, err := identity.NewIdentityClient(conn).GetCapabilities(ctx, &identity.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := capStrings(caps.GetCapabilities()), []string{"service:NODE_SERVICE", "reclaimspace:ONLINE"}; !slices.Equal(got, want) {
		t.Errorf("caps %v, want %v", got, want)
	}
	// Registered with the real fitrim: an unmounted directory is refused.
	_, err = reclaimspace.NewReclaimSpaceNodeClient(conn).NodeReclaimSpace(ctx, &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v", StagingTargetPath: t.TempDir()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("unmounted dir: %v, want FailedPrecondition", err)
	}
	_, err = reclaimspace.NewReclaimSpaceControllerClient(conn).ControllerReclaimSpace(ctx, &reclaimspace.ControllerReclaimSpaceRequest{VolumeId: "v"})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("controller service in node mode: %v, want Unimplemented", err)
	}
}

func TestServerControllerMode(t *testing.T) {
	conn := startServer(t, ModeController)
	ctx := context.Background()

	caps, err := identity.NewIdentityClient(conn).GetCapabilities(ctx, &identity.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := capStrings(caps.GetCapabilities()), []string{"service:CONTROLLER_SERVICE", "reclaimspace:OFFLINE"}; !slices.Equal(got, want) {
		t.Errorf("caps %v, want %v", got, want)
	}
	_, err = reclaimspace.NewReclaimSpaceControllerClient(conn).ControllerReclaimSpace(ctx, &reclaimspace.ControllerReclaimSpaceRequest{VolumeId: "v"})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("ControllerReclaimSpace: %v, want Unimplemented", err)
	}
	// The node service is not registered, so nothing is ever trimmed here.
	_, err = reclaimspace.NewReclaimSpaceNodeClient(conn).NodeReclaimSpace(ctx, &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v", StagingTargetPath: "/"})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("node service in controller mode: %v, want Unimplemented", err)
	}
}

// FITRIM is safe to abandon, so a stop must not wait for it beyond the timeout.
func TestShutdownAbandonsLongTrim(t *testing.T) {
	dir, err := os.MkdirTemp("", "crs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	lis, err := net.Listen("unix", filepath.Join(dir, "csi-addons.sock"))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	s := newServer(Options{DriverName: "d", Version: "v", Mode: ModeNode}, func(string) (uint64, error) {
		close(entered)
		<-release
		return 0, nil
	})
	served := make(chan error, 1)
	go func() { served <- s.Serve(lis) }()
	conn, err := grpc.NewClient("unix://"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go reclaimspace.NewReclaimSpaceNodeClient(conn).NodeReclaimSpace(context.Background(), &reclaimspace.NodeReclaimSpaceRequest{VolumeId: "v", StagingTargetPath: "/stage"})
	<-entered

	start := time.Now()
	s.Shutdown(200 * time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Shutdown took %v with a trim running, want about the 200ms timeout", d)
	}
	// The server finishes stopping once the abandoned RPC returns.
	close(release)
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the trim finished")
	}
}
