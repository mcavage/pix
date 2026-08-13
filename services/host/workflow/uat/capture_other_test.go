//go:build !unix

package uat

import (
	"testing"
)

func TestReadCaptureFileNoFollow_Other(t *testing.T) {
	_, err := readCaptureFileNoFollow("somepath")
	if err == nil {
		t.Fatal("expected error on unsupported platform")
	}
	if err.Error() != "unsupported platform: OAuth capture requires unix O_NOFOLLOW" {
		t.Fatalf("unexpected error: %v", err)
	}
}
