//go:build darwin

package lease

import (
	"fmt"
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// fcntlGetPath is <fcntl.h>'s F_GETPATH (0x32); the standard syscall package
// does not export darwin fcntl commands beyond F_GETFD/F_SETFD.
const fcntlGetPath = 0x32

// darwinPathMax is <sys/syslimits.h>'s PATH_MAX, the buffer F_GETPATH fills.
const darwinPathMax = 1024

// scanFDsForTargets resolves this process's own open file descriptors for the
// Darwin (BSD) fd table. macOS exposes /dev/fd exactly like Linux exposes
// /proc/self/fd — one entry per open fd number — but the entries are NOT
// symlinks: os.Readlink on /dev/fd/<n> fails (EINVAL), because fdesc mounts
// them as the actual open file, not a link to its path. So instead of
// reading a link, this asks the kernel directly what path backs each fd
// number via fcntl(fd, F_GETPATH, buf), the documented darwin mechanism for
// recovering an fd's path (see fcntl(2)).
func scanFDsForTargets(targets map[string]string) (map[string]bool, error) {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return nil, fmt.Errorf("readdir /dev/fd: %w", err)
	}
	found := map[string]bool{}
	for _, e := range entries {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // "." / ".." or anything non-numeric
		}
		path, ok := fcntlGetPathString(fd)
		if !ok {
			continue
		}
		for label, target := range targets {
			if target != "" && path == target {
				found[label] = true
			}
		}
	}
	return found, nil
}

// fcntlGetPathString wraps fcntl(fd, F_GETPATH, &buf) for fd, returning the
// NUL-terminated path fcntl wrote (ok=false when the syscall itself failed —
// e.g. the fd closed between ReadDir and here, or a non-path-backed fd like a
// socket).
func fcntlGetPathString(fd int) (string, bool) {
	buf := make([]byte, darwinPathMax)
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(fcntlGetPath), uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return "", false
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), true
}
