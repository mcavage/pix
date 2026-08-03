// movedfixture_test.go — fixtures that came with the tests moved from cmd/pix.
package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/sys/systest"
	"pix/host/workspace"
	"runtime"
	"testing"
	"time"
)

// hangingProbe returns a probe seam that execs the hanging binary under a
// SHORT injectable deadline — the real runWithTimeoutD path, so this proves
// the context deadline actually kills a wedged child.
func hangingProbe(t *testing.T, deadline time.Duration) func(string, ...string) (string, bool, error) {
	exe := hangingExe(t)
	return func(name string, args ...string) (string, bool, error) {
		return sys.RunTimed(deadline, exe, args...)
	}
}

func hangingExe(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake executable; unix-only test")
	}
	p := filepath.Join(t.TempDir(), "hang")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// withSandboxMCPStateDirFn overrides the seam for the duration of the test.
func withSandboxMCPStateDirFn(t *testing.T, fn func() (string, error)) {
	t.Helper()
	old := workspace.MCPStateDirFn
	workspace.MCPStateDirFn = fn
	t.Cleanup(func() { workspace.MCPStateDirFn = old })
}

// f2Env is a hostenv.Env where resolveOpRefs answers f2Refs (via PIX_CONFIG)
// and op/gog/pix-host all resolve canonically.
func f2Env() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		switch name {
		case "op":
			return f2Op, nil
		case "gog":
			return f2Gog, nil
		case "sbx":
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("%q not found", name)
	}, GetenvFn: func(k string) string {
		if k == "PIX_CONFIG" {
			return "/fake/pix/config.toml"
		}
		return ""
	}, IsFileFn: func(p string) bool { return p == f2Refs }}, HostBinary: func() (string, error) { return f2Host, nil }}
}

const (
	f2Refs = "/fake/pix/op-refs.env"
	f2Op   = "/usr/local/bin/op"
	f2Gog  = "/usr/local/bin/gog"
	f2Host = "/usr/local/bin/pix-host"
)

// writeFile / exists / mustCreateReceipt-style helpers: three lines each, copied
// rather than shared. See the note in cmd/pix's own fixture files.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
