package main

import (
	"errors"
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/sys/systest"
	"strings"
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

func TestPathShadowIssue(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "local", "bin")
	brew := filepath.Join(root, "homebrew", "bin")
	cellar := filepath.Join(root, "homebrew", "Cellar", "pix", "0.1.8", "bin")
	for _, dir := range []string{local, brew, cellar} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	localPix := filepath.Join(local, "pix")
	brewPix := filepath.Join(brew, "pix")
	cellarPix := filepath.Join(cellar, "pix")
	for _, path := range []string{localPix, cellarPix} {
		if err := os.WriteFile(path, []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(cellarPix, brewPix); err != nil {
		t.Fatal(err)
	}
	pathValue := local + string(os.PathListSeparator) + brew + string(os.PathListSeparator) + local
	getenv := func(key string) string {
		if key == "PATH" {
			return pathValue
		}
		return ""
	}

	got := pathShadowIssue("pix", localPix, getenv)
	for _, want := range []string{"multiple pix installations", localPix, brewPix, "remove the stale copy explicitly"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "rm -rf") {
		t.Fatalf("message contains destructive fix: %q", got)
	}

	got = pathShadowIssue("pix", cellarPix, getenv)
	if !strings.Contains(got, "not the running binary") || !strings.Contains(got, localPix) {
		t.Fatalf("direct Cellar invocation should report the PATH shadow: %q", got)
	}
}

func TestInstallDuplicatesGroupSurfacesDoctorWarning(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	second := filepath.Join(root, "second")
	for _, dir := range []string{first, second} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pix"), []byte("binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	self := filepath.Join(first, "pix")
	env := hostenv.Env{System: &systest.Fake{ExecutableFn: func() (string, error) { return self, nil }, GetenvFn: func(key string) string {
		if key == "PATH" {
			return first + string(os.PathListSeparator) + second
		}
		return ""
	}}}
	g := installDuplicatesGroup(env)
	if len(g.Checks) != 1 || g.Checks[0].Result() != readiness.VerdictTodo {
		t.Fatalf("group = %+v", g)
	}
	if !strings.Contains(g.Checks[0].Detail, "multiple pix installations") {
		t.Fatalf("detail = %q", g.Checks[0].Detail)
	}
}
