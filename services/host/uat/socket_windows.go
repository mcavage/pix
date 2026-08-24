//go:build windows

package uat

import (
	"errors"
	"net"
	"os"
	"time"
)

var errUatSocketUnsupported = errors.New("the UAT worker/gateway Unix-domain socket relay requires a Unix host")

func checkOwnedByCurrentUser(dir string, info os.FileInfo) error {
	return errUatSocketUnsupported
}

// ListenSocket has no supported implementation on Windows: the UAT
// self-development runner (docs/design/self-development-uat.md) is a
// macOS/Linux host feature.
func ListenSocket(path string) (net.Listener, error) {
	return nil, errUatSocketUnsupported
}

// DialSocket has no supported implementation on Windows; see ListenSocket.
func DialSocket(path string, attempts int, delay time.Duration) (net.Conn, error) {
	return nil, errUatSocketUnsupported
}
