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
	defer unix.Close(fd)

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
			return nil, err
		}

		unix.Close(fd)
		fd = nextFd
	}

	// Create os.File from fd
	return os.NewFile(uintptr(fd), relPath), nil
}
