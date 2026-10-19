//go:build functional

// Package functional runs the real binary against a real kernel: a loop-device
// ext4 filesystem and FITRIM through the gRPC API. Needs root (loop devices,
// mount); CI runs it with sudo.
package functional

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/csi-addons/spec/lib/go/identity"
	"github.com/csi-addons/spec/lib/go/reclaimspace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

var bin string

func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "functional tests need root (loop devices, mount)")
		os.Exit(1)
	}
	dir, err := os.MkdirTemp("", "crs-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	bin = filepath.Join(dir, "csi-reclaimspace")
	// -buildvcs=false: as root on a checkout owned by another user, git refuses
	// to report VCS status and the build fails.
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "..").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// allocated returns the bytes a file occupies on its backing filesystem.
func allocated(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

type sidecar struct {
	cmd  *exec.Cmd
	conn *grpc.ClientConn
}

// start runs the binary on sock in mode and waits until it answers Probe.
func start(t *testing.T, sock, mode string) *sidecar {
	t.Helper()
	return startUnder(t, sock, mode)
}

// startUnder runs the binary through prefix (e.g. setpriv) on sock in mode.
func startUnder(t *testing.T, sock, mode string, prefix ...string) *sidecar {
	t.Helper()
	args := append(prefix, bin)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "DRIVER_NAME=test.csi.example.com", "CSI_ADDONS_SOCKET="+sock, "MODE="+mode)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	})
	conn, err := grpc.NewClient("unix://"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for {
		_, err := identity.NewIdentityClient(conn).Probe(ctx, &identity.ProbeRequest{})
		if err == nil {
			return &sidecar{cmd: cmd, conn: conn}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("sidecar on %s did not answer Probe: %v", sock, err)
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// socketPath returns a short socket path (unix sockets are limited to ~108 bytes).
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "crs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "csi-addons.sock")
}

// documentedCaps is the capability set the README's securityContext grants.
const documentedCaps = "+sys_admin,+dac_read_search"

// volume is a 1G ext4 on a loop device, mounted at stage like a staged CSI volume.
type volume struct {
	img, loop, stage string
	mounted          bool
}

func newVolume(t *testing.T) *volume {
	t.Helper()
	work := t.TempDir()
	v := &volume{img: filepath.Join(work, "vol.img"), stage: filepath.Join(work, "stage")}
	run(t, "truncate", "-s", "1G", v.img)
	run(t, "mkfs.ext4", "-q", v.img)
	v.loop = run(t, "losetup", "-f", "--show", v.img)
	t.Cleanup(func() { exec.Command("losetup", "-d", v.loop).Run() })
	if err := os.Mkdir(v.stage, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, "mount", v.loop, v.stage)
	v.mounted = true
	t.Cleanup(func() {
		if v.mounted {
			exec.Command("umount", v.stage).Run()
		}
	})
	return v
}

func reclaim(s *sidecar, path string) error {
	_, err := reclaimspace.NewReclaimSpaceNodeClient(s.conn).NodeReclaimSpace(context.Background(),
		&reclaimspace.NodeReclaimSpaceRequest{VolumeId: "vol-1", StagingTargetPath: path})
	return err
}

func TestReclaimFreesBackingBlocks(t *testing.T) {
	v := newVolume(t)
	img, loop, stage := v.img, v.loop, v.stage

	keep, junk := filepath.Join(stage, "keep"), filepath.Join(stage, "junk")
	run(t, "dd", "if=/dev/urandom", "of="+keep, "bs=1M", "count=20", "status=none")
	run(t, "dd", "if=/dev/urandom", "of="+junk, "bs=1M", "count=200", "status=none")
	run(t, "sync")
	keepSum := run(t, "md5sum", keep)
	if err := os.Remove(junk); err != nil {
		t.Fatal(err)
	}
	run(t, "sync")
	before := allocated(t, img)

	s := start(t, socketPath(t), "node")
	if err := reclaim(s, stage); err != nil {
		t.Fatalf("NodeReclaimSpace: %v", err)
	}
	run(t, "sync")
	after := allocated(t, img)
	t.Logf("backing file allocation %d -> %d bytes", before, after)
	if before-after < 150<<20 {
		t.Fatalf("backing file allocation %d -> %d bytes, want at least 150 MiB freed", before, after)
	}
	if got := run(t, "md5sum", keep); got != keepSum {
		t.Fatalf("kept data changed: %s -> %s", keepSum, got)
	}
	run(t, "umount", stage)
	v.mounted = false
	run(t, "e2fsck", "-fn", loop)
}

// Volumes whose root an app chowned to itself with mode 0700 (databases do)
// must still be trimmed with only the capabilities the README documents.
func TestDocumentedCapabilitiesTrimNonRootOwnedVolume(t *testing.T) {
	v := newVolume(t)
	if err := os.Chown(v.stage, 999, 999); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(v.stage, 0o700); err != nil {
		t.Fatal(err)
	}
	// --inh-caps=-all: otherwise inherited caps would survive the exec and the
	// test would not prove the documented set is enough.
	s := startUnder(t, socketPath(t), "node", "setpriv", "--inh-caps=-all", "--bounding-set=-all,"+documentedCaps, "--")
	if err := reclaim(s, v.stage); err != nil {
		t.Fatalf("NodeReclaimSpace with %s: %v", documentedCaps, err)
	}
}

// A directory inside a volume is not its mount root: trimming from there is refused.
func TestSubdirectoryOfVolumeIsRefused(t *testing.T) {
	v := newVolume(t)
	sub := filepath.Join(v.stage, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reclaim(start(t, socketPath(t), "node"), sub); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("subdirectory: %v, want FailedPrecondition", err)
	}
}

// A bind mount is a mount root even on the same filesystem; drivers that
// stage with bind mounts must be trimmed, not refused.
func TestBindMountIsTrimmed(t *testing.T) {
	v := newVolume(t)
	// Both ends on the volume itself, so the bind shares its parent's st_dev.
	data, bind := filepath.Join(v.stage, "data"), filepath.Join(v.stage, "bind")
	for _, d := range []string{data, bind} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	run(t, "mount", "--bind", data, bind)
	t.Cleanup(func() { exec.Command("umount", bind).Run() })
	if err := reclaim(start(t, socketPath(t), "node"), bind); err != nil {
		t.Fatalf("bind mount: %v", err)
	}
}

func TestUnmountedPathIsNotTrimmed(t *testing.T) {
	if err := reclaim(start(t, socketPath(t), "node"), t.TempDir()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unmounted dir: %v, want FailedPrecondition", err)
	}
}

func TestControllerMode(t *testing.T) {
	s := start(t, socketPath(t), "controller")
	id, err := identity.NewIdentityClient(s.conn).GetIdentity(context.Background(), &identity.GetIdentityRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if id.GetName() != "test.csi.example.com" || id.GetVendorVersion() == "" {
		t.Errorf("identity %q %q", id.GetName(), id.GetVendorVersion())
	}
	caps, err := identity.NewIdentityClient(s.conn).GetCapabilities(context.Background(), &identity.GetCapabilitiesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range caps.GetCapabilities() {
		got = append(got, c.String())
	}
	if !slices.ContainsFunc(got, func(s string) bool { return strings.Contains(s, "OFFLINE") }) {
		t.Errorf("controller caps %v, want ReclaimSpace OFFLINE", got)
	}
}

func TestMissingConfigExitsNonZero(t *testing.T) {
	cmd := exec.Command(bin)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "DRIVER_NAME") {
		t.Fatalf("exit %v, output %q; want status 1 naming DRIVER_NAME", err, out)
	}
	cmd = exec.Command(bin)
	cmd.Env = []string{"DRIVER_NAME=x", "MODE=node", "CSI_ADDONS_SOCKET=/nonexistent/dir/csi-addons.sock"}
	out, err = cmd.CombinedOutput()
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "/nonexistent/dir") {
		t.Fatalf("exit %v, output %q; want status 1 naming the socket path", err, out)
	}
}

func TestSigtermExitsCleanly(t *testing.T) {
	s := start(t, socketPath(t), "node")
	if err := s.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := s.cmd.Wait(); err != nil {
		t.Fatalf("exit after SIGTERM: %v, want status 0", err)
	}
}

func TestRestartAfterKill(t *testing.T) {
	sock := socketPath(t)
	s := start(t, sock, "node")
	s.cmd.Process.Kill()
	s.cmd.Wait()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("killed process left no socket file to replace: %v", err)
	}
	start(t, sock, "node")
}
