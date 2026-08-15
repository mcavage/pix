//go:build unix
// +build unix

package uat

import (
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

func openSafeNoSymlink(root string, relPath string) (*os.File, error) {
	parts := strings.Split(filepath.ToSlash(relPath), "/")

	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}

	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}

		flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC
		if i < len(parts)-1 {
			flags |= unix.O_DIRECTORY
		}

		nextFd, err := unix.Openat(fd, part, flags, 0)
		if err != nil {
			_ = unix.Close(fd)
			return nil, err
		}

		_ = unix.Close(fd)
		fd = nextFd
	}

	// Ownership of the final descriptor transfers to os.File. Do not defer a
	// close on the original integer: openat may reuse that descriptor number for
	// the final file after an intermediate close.
	return os.NewFile(uintptr(fd), relPath), nil
}
