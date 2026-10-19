package main

import (
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadConfig(t *testing.T) {
	ok := map[string]string{"DRIVER_NAME": "cinder.csi.openstack.org", "CSI_ADDONS_SOCKET": "/csi/csi-addons.sock", "MODE": "node"}
	if _, err := loadConfig(env(ok)); err != nil {
		t.Fatalf("valid node config: %v", err)
	}
	ctrl := maps.Clone(ok)
	ctrl["MODE"] = "controller"
	if _, err := loadConfig(env(ctrl)); err != nil {
		t.Fatalf("valid controller config: %v", err)
	}
	for _, tc := range []struct{ name, key, value string }{
		{"no driver name", "DRIVER_NAME", ""},
		{"no socket", "CSI_ADDONS_SOCKET", ""},
		{"relative socket", "CSI_ADDONS_SOCKET", "csi-addons.sock"},
		{"no mode", "MODE", ""},
		{"unknown mode", "MODE", "both"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := maps.Clone(ok)
			m[tc.key] = tc.value
			if _, err := loadConfig(env(m)); err == nil {
				t.Fatalf("want an error for %s=%q", tc.key, tc.value)
			}
		})
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "crs") // short: unix socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "csi-addons.sock")

	// A crashed run leaves the socket file behind.
	first, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	first.Close()
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("stale socket not left behind: %v", err)
	}

	lis, err := listen(sock)
	if err != nil {
		t.Fatalf("listen over a stale socket: %v", err)
	}
	lis.Close()
}

func TestListenMissingDir(t *testing.T) {
	if _, err := listen(filepath.Join(t.TempDir(), "missing", "csi-addons.sock")); err == nil {
		t.Fatal("want an error when the socket directory does not exist")
	}
}

// shortDir returns a directory short enough for unix socket paths.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "crs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// A mistyped CSI_ADDONS_SOCKET must never delete what is at that path.
func TestListenRefusesRegularFile(t *testing.T) {
	path := filepath.Join(shortDir(t), "csi.sock")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := listen(path); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatalf("got %v, want a 'not a socket' error", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "keep" {
		t.Fatalf("file was touched: %q, %v", b, err)
	}
}

// Another process's live socket (e.g. the driver's csi.sock) is left alone.
func TestListenRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "csi.sock")
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	if _, err := listen(path); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("got %v, want an 'in use' error", err)
	}
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("live socket no longer answers: %v", err)
	}
	c.Close()
}

// Only a socket nobody answers on (ECONNREFUSED) is stale; one this process
// cannot even connect to (EACCES) may be someone else's live socket.
func TestListenRefusesUnreachableSocket(t *testing.T) {
	path := filepath.Join(shortDir(t), "csi.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the socket's permissions")
	}
	if _, err := listen(path); err == nil {
		t.Fatal("want an error for a socket that cannot be reached")
	}
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("socket was removed: %v", err)
	}
}
