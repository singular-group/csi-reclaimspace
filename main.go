// csi-reclaimspace serves the CSI-Addons ReclaimSpace API next to a CSI driver,
// so CSI-Addons ReclaimSpaceJobs can trim the driver's filesystem volumes.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"csi-reclaimspace/internal/reclaimspace"
)

// version is set at build time: -ldflags "-X main.version=<x.y.z.N>".
var version = "dev"

type config struct {
	driverName string
	socket     string
	mode       reclaimspace.Mode
}

// loadConfig reads and validates the environment through getenv.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
		driverName: getenv("DRIVER_NAME"),
		socket:     getenv("CSI_ADDONS_SOCKET"),
		mode:       reclaimspace.Mode(getenv("MODE")),
	}
	if c.driverName == "" {
		return c, errors.New("DRIVER_NAME is required: the CSI driver name, e.g. cinder.csi.openstack.org")
	}
	if !filepath.IsAbs(c.socket) {
		return c, fmt.Errorf("CSI_ADDONS_SOCKET must be an absolute socket path, got %q", c.socket)
	}
	if c.mode != reclaimspace.ModeNode && c.mode != reclaimspace.ModeController {
		return c, fmt.Errorf("MODE must be %q or %q, got %q", reclaimspace.ModeNode, reclaimspace.ModeController, c.mode)
	}
	return c, nil
}

// listen replaces only a socket left by a previous run of the container: a
// mistyped path must not delete another file or a live socket such as the
// driver's own.
func listen(socket string) (net.Listener, error) {
	fi, err := os.Lstat(socket)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case fi.Mode().Type() != fs.ModeSocket:
		return nil, fmt.Errorf("%s exists and is not a socket", socket)
	default:
		c, err := net.DialTimeout("unix", socket, time.Second)
		if err == nil {
			c.Close()
			return nil, fmt.Errorf("%s is in use by another process", socket)
		}
		// Only "nobody listens" proves it stale; any other error may hide a
		// live socket this process just cannot reach.
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%s exists and its state is unknown: %w", socket, err)
		}
		if err := os.Remove(socket); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", socket, err)
		}
	}
	return net.Listen("unix", socket)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "csi-reclaimspace: %v\n", err)
	os.Exit(1)
}

func main() {
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		fail(err)
	}
	lis, err := listen(cfg.socket)
	if err != nil {
		fail(err)
	}
	srv := reclaimspace.NewServer(reclaimspace.Options{DriverName: cfg.driverName, Version: version, Mode: cfg.mode})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	served := make(chan error, 1)
	go func() { served <- srv.Serve(lis) }()

	log.Printf("csi-reclaimspace %s: %s mode for %s on %s", version, cfg.mode, cfg.driverName, cfg.socket)
	select {
	case err := <-served:
		fail(fmt.Errorf("serve: %w", err))
	case <-ctx.Done():
		// Returning exits the process, abandoning a FITRIM that outlived the wait.
		srv.Shutdown(10 * time.Second)
	}
}
