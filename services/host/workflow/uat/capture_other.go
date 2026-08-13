//go:build !unix

package uat

import (
	"os"
)

func readCaptureFileNoFollow(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", os.ErrPermission
	}
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()

	data := make([]byte, 8192)
	n, _ := f.Read(data)
	if n > 0 {
		return string(data[:n]), nil
	}
	return "", nil
}
