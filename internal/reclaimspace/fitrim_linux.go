package reclaimspace

import (
	"errors"
	"io/fs"
	"math"
	"unsafe"

	"golang.org/x/sys/unix"
)

// errNotMountPoint is returned when the path is not the root of a mount.
var errNotMountPoint = errors.New("not a mount point")

// fitrimIoctl is FITRIM, _IOWR('X', 121, struct fstrim_range), which
// golang.org/x/sys/unix does not define. Same value on every Linux arch.
const fitrimIoctl = 0xC0185879

// fstrimRange mirrors struct fstrim_range from <linux/fs.h>.
type fstrimRange struct {
	start  uint64
	length uint64
	minLen uint64
}

// fitrim discards every unused block of the filesystem mounted at path and
// returns the byte count the kernel reports as trimmed.
func fitrim(path string) (uint64, error) {
	// A mount root is a directory; O_DIRECTORY refuses FIFOs and device nodes
	// before opening them, which could block or have side effects.
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOTDIR) {
		return 0, errNotMountPoint
	}
	if err != nil {
		return 0, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	return fitrimMountRoot(fd)
}

// fitrimMountRoot issues FITRIM on fd only if the kernel reports fd as the root
// of a mount. The open fd pins that mount, so the check cannot race with an
// unmount.
func fitrimMountRoot(fd int) (uint64, error) {
	var stx unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH, unix.STATX_BASIC_STATS, &stx); err != nil {
		return 0, err
	}
	if stx.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT == 0 {
		return 0, errors.New("the kernel does not report mount roots (STATX_ATTR_MOUNT_ROOT needs Linux 5.8+)")
	}
	if stx.Attributes&unix.STATX_ATTR_MOUNT_ROOT == 0 {
		return 0, errNotMountPoint
	}

	r := fstrimRange{length: math.MaxUint64}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), fitrimIoctl, uintptr(unsafe.Pointer(&r))); errno != 0 {
		return 0, errno
	}
	return r.length, nil
}
