//go:build windows
// +build windows

package uat

import (
	"errors"
	"os"
)

func openSafeNoSymlink(root string, relPath string) (*os.File, error) {
	return nil, errors.New("atomic no-symlink artifact reads require a Unix host")
}
