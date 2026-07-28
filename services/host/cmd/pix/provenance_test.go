package main

import (
	"errors"
	"testing"
)

func TestDetectInstallChannel(t *testing.T) {
	originalVersion := version
	t.Cleanup(func() { version = originalVersion })

	tests := []struct {
		name       string
		version    string
		self       string
		resolved   string
		prefix     string
		execErr    error
		want       installChannel
		wantString string
	}{
		{name: "Homebrew arm prefix", version: "0.3.0", self: "/opt/homebrew/bin/pix", resolved: "/opt/homebrew/Cellar/pix/0.3.0/bin/pix", prefix: "/opt/homebrew", want: channelHomebrew, wantString: "Homebrew"},
		{name: "installer", version: "0.3.0", self: "/home/x/.local/bin/pix", resolved: "/home/x/.local/bin/pix", want: channelInstaller, wantString: "Installer"},
		{name: "local build wins regardless of path", version: "dev", self: "/opt/homebrew/bin/pix", resolved: "/opt/homebrew/Cellar/pix/0.3.0/bin/pix", prefix: "/opt/homebrew", want: channelLocalDev, wantString: "LocalDev"},
		{name: "executable error", version: "0.3.0", execErr: errors.New("boom"), want: channelUnknown, wantString: "?"},
		{name: "Cellar and pix must be adjacent", version: "0.3.0", self: "/tmp/Cellar/other/pix/0.3.0/bin/pix", resolved: "/tmp/Cellar/other/pix/0.3.0/bin/pix", want: channelInstaller, wantString: "Installer"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			version = tc.version
			got := detectInstallChannel(
				func() (string, error) { return tc.self, tc.execErr },
				func(string) (string, error) { return tc.resolved, nil },
				func(key string) string {
					if key == "HOMEBREW_PREFIX" {
						return tc.prefix
					}
					return ""
				},
			)
			if got.Channel != tc.want || got.Channel.String() != tc.wantString {
				t.Fatalf("channel = %v (%q), want %v (%q); provenance: %+v", got.Channel, got.Channel.String(), tc.want, tc.wantString, got)
			}
		})
	}
}
