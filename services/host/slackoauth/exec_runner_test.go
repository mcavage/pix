package slackoauth

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestExecRunnerPipesStdinAndCapturesStdout proves ExecRunner writes stdin to
// the child's standard input and returns exactly what it printed on stdout,
// with the secret payload never appearing on the command line.
func TestExecRunnerPipesStdinAndCapturesStdout(t *testing.T) {
	var r ExecRunner
	secret := "top-secret-blob-payload"
	out, err := r.Run(context.Background(), []byte(secret), "sh", "-c", "cat")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != secret {
		t.Errorf("stdout = %q, want %q", out, secret)
	}
}

// TestExecRunnerNoStdinWhenNil proves a nil/empty stdin never blocks waiting
// for input the caller didn't intend to send.
func TestExecRunnerNoStdinWhenNil(t *testing.T) {
	var r ExecRunner
	out, err := r.Run(context.Background(), nil, "sh", "-c", "echo hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(string(out)) != "hello" {
		t.Errorf("stdout = %q, want hello", out)
	}
}

// TestExecRunnerContextTimeoutKillsChild proves ctx cancellation actually
// terminates a long-running child instead of leaking it, and returns
// promptly rather than waiting out the child's own sleep.
func TestExecRunnerContextTimeoutKillsChild(t *testing.T) {
	var r ExecRunner
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	// Force a descendant to inherit stdout/stderr. Killing only the shell leaves
	// that descendant holding the capture pipes open, which made Cmd.Wait block
	// for the full five seconds on GitHub's Ubuntu runner.
	_, err := r.Run(ctx, nil, "sh", "-c", "sleep 5 & wait")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Run succeeded despite context timeout; want an error")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Run took %v to return after a 100ms timeout; child was not killed promptly", elapsed)
	}
}

// TestExecRunnerErrorNeverContainsStdinSecret proves that when the child
// fails, ExecRunner's own error text never includes the stdin payload — even
// though op's own diagnostics are out of our control, OUR error wrapping
// must never echo what was piped in.
func TestExecRunnerErrorNeverContainsStdinSecret(t *testing.T) {
	var r ExecRunner
	secret := "TOPSECRET-blob-should-never-leak"
	_, err := r.Run(context.Background(), []byte(secret), "sh", "-c", "cat >/dev/null; echo synthetic-op-error >&2; exit 1")
	if err == nil {
		t.Fatal("Run succeeded despite a non-zero exit; want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked the stdin secret: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "synthetic-op-error") {
		t.Errorf("error = %q, want it to surface the child's stderr diagnostic", err.Error())
	}
}
