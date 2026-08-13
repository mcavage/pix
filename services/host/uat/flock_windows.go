//go:build windows

package uat

func lockFile(fd uintptr) error {
	return nil
}

func unlockFile(fd uintptr) error {
	return nil
}
