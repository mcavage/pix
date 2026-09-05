package main

import (
	"fmt"
	"os"
	"path/filepath"
	"pix/host/workflow/launch"
	"runtime"
	"strings"
	"testing"
	"time"

	"pix/host/hostenv"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/sys/systest"
)

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

// hangingProbe returns a probe seam that execs the hanging binary under a
// SHORT injectable deadline — the real runWithTimeoutD path, so this proves
// the context deadline actually kills a wedged child.
func hangingProbe(t *testing.T, deadline time.Duration) func(string, ...string) (string, bool, error) {
	exe := hangingExe(t)
	return func(name string, args ...string) (string, bool, error) {
		return sys.RunTimed(deadline, exe, args...)
	}
}

func TestRunWithTimeoutD_HangingProcessBounded(t *testing.T) {
	exe := hangingExe(t)
	start := time.Now()
	_, timedOut, _ := sys.RunTimed(100*time.Millisecond, exe)
	if !timedOut {
		t.Fatal("a hanging process must report timedOut under the injected deadline")
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("runWithTimeoutD took %s — the deadline did not bound the child", el)
	}
}

func TestProbeSbxSecrets_HangingSbxIsErrorNotAbsent(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunTimedFn: hangingProbe(t, 100*time.Millisecond)}}
	start := time.Now()
	_, state := secret.ProbeSbxSecrets(env)
	if state != secret.SbxSecretsError {
		t.Errorf("hanging `sbx secret ls` must classify secret.SbxSecretsError, got %v", state)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("secret.ProbeSbxSecrets took %s — unbounded", el)
	}
}

// TestConfiguredModelKeyState_UnreadableRefsUnknownProceeds pins run's
// launch-preflight tri-state when the evidence cannot be read: probeOK=false
// (unknown), which under the existing rule PROCEEDS — only a POSITIVELY
// answered "no model ref is configured" blocks a launch.
func TestConfiguredModelKeyState_UnreadableRefsUnknownProceeds(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil },
		IsFileFn:   func(string) bool { return true },
		ReadFileFn: func(string) (string, error) { return "", fmt.Errorf("permission denied") }}}
	present, probeOK := launch.ConfiguredModelKeyState(env)
	if present || probeOK {
		t.Errorf("unreadable refs must be (present=false, probeOK=false) so run proceeds, got (%v,%v)", present, probeOK)
	}
}

// --- finding 13: mcp load argument validation --------------------------------

// TestMcpLoadSandbox_IsRunsOwnDefaultName: `pix mcp load NAME [DIR]` must
// target the SAME box `pix run DIR` would, and it derives that name rather than
// looking it up — U04e deleted the receipt store the old resolver scanned, and
// with it the stale "pix-<basename>" fallback that named a sandbox nothing
// creates. One derivation shared by both commands is what makes them agree.
func TestMcpLoadSandbox_IsRunsOwnDefaultName(t *testing.T) {
	t.Setenv("PIX_HOME", t.TempDir())
	ws := t.TempDir()
	got, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	again, err := resolveSandboxName("", ws)
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	if got != again {
		t.Error("the derivation must be stable for one workspace")
	}
	other, err := resolveSandboxName("", t.TempDir())
	if err != nil {
		t.Fatalf("resolveSandboxName: %v", err)
	}
	if other == got {
		t.Error("two different workspaces must not derive one sandbox name")
	}
	if !strings.HasPrefix(got, "pix-") {
		t.Errorf("derived sandbox %q must be in the pix-* scope pix rm owns", got)
	}
}
