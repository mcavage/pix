//go:build darwin

package lease

import (
	"fmt"
	"syscall"
	"unsafe"
)

// fcntlGetPath is <fcntl.h>'s F_GETPATH (0x32); the standard syscall package
// does not export darwin fcntl commands beyond F_GETFD/F_SETFD.
const fcntlGetPath = 0x32

// darwinPathMax is <sys/syslimits.h>'s PATH_MAX, the buffer F_GETPATH fills.
const darwinPathMax = 1024

// darwinMaxFDScan bounds the numeric fd range this ever probes, independent
// of whatever RLIMIT_NOFILE reports. A soft limit can be "unlimited" or set
// to a very large number without a single fd actually being open that high,
// so this keeps a misconfigured limit from turning one self-check into a
// million-iteration syscall loop.
const darwinMaxFDScan = 65536

// scanFDsForTargets resolves this process's own open file descriptors on
// Darwin (BSD) by walking the numeric fd space directly, NOT by reading
// /dev/fd as a directory. macOS's fdesc mount exposes /dev/fd/<n> as the
// open file itself rather than a symlink to it (os.Readlink fails EINVAL),
// and worse, os.ReadDir over /dev/fd stats each entry via fstatat against
// the SAME fd number it is enumerating: an entry that closes or gets
// reused between the readdir() and the fstatat() (which happens routinely —
// short-lived pipes, a concurrent goroutine's own Open/Close, this very
// scan's own directory fd) fails with a bad-fd error and can abort the
// whole scan. Probing fd numbers with fcntl(2) has no such race: F_GETFD
// either succeeds (the fd is open right now) or fails EBADF (it is not),
// and that answer is self-consistent because it comes from one syscall
// against one fd, not a directory snapshot paired with a second lookup.
//
// For each fd F_GETFD reports open, fcntl(fd, F_GETPATH, buf) recovers the
// backing path — the documented darwin mechanism for turning an fd number
// into a path (see fcntl(2)) — and is compared against the requested
// targets.
func scanFDsForTargets(targets map[string]string) (map[string]bool, error) {
	limit, err := darwinFDScanLimit()
	if err != nil {
		return nil, fmt.Errorf("getrlimit NOFILE: %w", err)
	}
	found := map[string]bool{}
	for fd := 0; fd < limit; fd++ {
		if !fdIsOpen(fd) {
			continue
		}
		path, ok := fcntlGetPathString(fd)
		if !ok {
			continue // not path-backed (e.g. a socket) or raced closed
		}
		for label, target := range targets {
			if target != "" && path == target {
				found[label] = true
			}
		}
	}
	return found, nil
}

// darwinFDScanLimit resolves how many fd numbers scanFDsForTargets should
// probe: this process's current RLIMIT_NOFILE soft limit, capped by
// capFDScanLimit (see lock_process_fds_common_test.go) so an
// unlimited/huge limit can never blow up the scan.
func darwinFDScanLimit() (int, error) {
	var rlim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rlim); err != nil {
		return 0, err
	}
	return capFDScanLimit(rlim.Cur, darwinMaxFDScan), nil
}

// fdIsOpen reports whether fd currently names an open descriptor in this
// process, via fcntl(fd, F_GETFD): the kernel answers EBADF for a closed or
// never-opened fd number and the descriptor's flags word otherwise. This is
// the direct kernel query that replaces trusting /dev/fd directory
// metadata.
func fdIsOpen(fd int) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	return errno == 0
}

// fcntlGetPathString wraps fcntl(fd, F_GETPATH, &buf) for fd, returning the
// NUL-terminated path fcntl wrote (ok=false when the syscall itself failed —
// e.g. the fd closed between fdIsOpen and here, or a non-path-backed fd like
// a socket).
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
