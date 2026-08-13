//go:build !unix

package uat

import (
	"errors"
)

func readCaptureFileNoFollow(path string) (string, error) {
	return "", errors.New("unsupported platform: OAuth capture requires unix O_NOFOLLOW")
}
