//go:build unix

package uat

import (
	"os"
	"syscall"
)

func readCaptureFileNoFollow(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
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
