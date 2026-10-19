package reclaimspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// _IOWR('X', 121, struct fstrim_range) with the generic Linux ioctl encoding.
func TestFitrimIoctlValue(t *testing.T) {
	const iocWrite, iocRead = 1, 2
	size := uint64(unsafe.Sizeof(fstrimRange{}))
	want := uint64(iocRead|iocWrite)<<30 | size<<16 | uint64('X')<<8 | 121
	if size != 24 || uint64(fitrimIoctl) != want {
		t.Fatalf("size %d, ioctl %#x; want 24 and %#x", size, uint64(fitrimIoctl), want)
	}
}

func TestFitrimMissingPath(t *testing.T) {
	_, err := fitrim(filepath.Join(t.TempDir(), "gone"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("got %v, want fs.ErrNotExist", err)
	}
}

// A plain directory is not a mount root. Getting the sentinel rather than the
// kernel's EPERM/ENOTTY/EOPNOTSUPP shows the ioctl was never issued.
func TestFitrimNotMountRoot(t *testing.T) {
	_, err := fitrim(t.TempDir())
	if !errors.Is(err, errNotMountPoint) {
		t.Fatalf("got %v, want errNotMountPoint", err)
	}
}

func TestFitrimRegularFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := fitrim(f)
	if !errors.Is(err, errNotMountPoint) {
		t.Fatalf("got %v, want errNotMountPoint", err)
	}
}

// /proc is a mount root, so the ioctl is issued; procfs rejects it with
// ENOTTY without touching any block device.
func TestFitrimMountRoot(t *testing.T) {
	_, err := fitrim("/proc")
	if !errors.Is(err, unix.ENOTTY) {
		t.Fatalf("got %v, want ENOTTY", err)
	}
}

// A FIFO (or device node) at the path must be refused without opening it for
// I/O: a plain open of a FIFO blocks until a writer appears.
func TestFitrimFifo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fifo")
	if err := unix.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := fitrim(p)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errNotMountPoint) {
			t.Fatalf("got %v, want errNotMountPoint", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fitrim blocked on a FIFO")
	}
}
