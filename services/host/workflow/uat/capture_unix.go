//go:build unix

package uat

import (
	"io"
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
	n, err := f.Read(data)
	if err != nil && err != io.EOF {
		return "", err
	}
	if n > 0 {
		return string(data[:n]), nil
	}
	return "", nil
}
