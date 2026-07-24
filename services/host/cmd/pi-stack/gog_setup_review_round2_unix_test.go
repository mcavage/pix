//go:build unix

package main

// gog_setup_review_round2_unix_test.go covers R2-05 on POSIX platforms:
// gogSetup's credentials-path check must reject a FIFO and a device file
// (Mode().IsRegular() must be false for both), while still allowing a
// symlink that resolves to a genuine regular file. FIFOs (syscall.Mkfifo)
// and /dev/null are POSIX-only concepts, hence the `unix` build tag (Go
// 1.19+'s unix-family constraint, same pattern as serve_start_unix_test.go);
// this file simply doesn't build on Windows rather than needing a stubbed
// counterpart, since gogSetup's own fileMode check has no platform-specific
// code (it's a plain os.Stat wrapper on every OS).

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// gogSetupRealFileModeEnv builds a gogSetup environment that wires REAL
// os.Stat-backed fileMode/statFile (rather than gogTestEnv's map-driven
// fakes), so a genuine FIFO/device on disk is actually exercised through the
// same code path defaultShellEnv() uses in production.
func gogSetupRealFileModeEnv(t *testing.T) shellEnv {
	t.Helper()
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		sbxRegisterOK: true,
	}
	env := ge.env()
	env.fileMode = func(path string) (os.FileMode, bool) {
		fi, err := os.Stat(path)
		if err != nil {
			return 0, false
		}
		return fi.Mode(), true
	}
	return env
}

// TestGogSetup_R205_FIFOCredentialsRejected: a FIFO at the credentials path
// (same size-zero, "exists and isn't a directory" shape a bare
// os.Stat().IsDir() check would have wrongly accepted) must be rejected —
// Mode().IsRegular() is false for a FIFO.
func TestGogSetup_R205_FIFOCredentialsRejected(t *testing.T) {
	gogSetupTestCfg(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "client.json")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	env := gogSetupRealFileModeEnv(t)
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: fifo}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the credentials path is a FIFO")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("expected the regular-file guidance, got %q", err)
	}
}

// TestGogSetup_R205_DevNullCredentialsRejected: /dev/null is a character
// device — "exists, zero size, not a directory" but definitely not a
// regular file. Every POSIX system has it, so this needs no fixture setup.
func TestGogSetup_R205_DevNullCredentialsRejected(t *testing.T) {
	gogSetupTestCfg(t)
	env := gogSetupRealFileModeEnv(t)
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: "/dev/null"}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the credentials path is /dev/null")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("expected the regular-file guidance, got %q", err)
	}
}

// TestGogSetup_R205_SymlinkToRegularFileAllowed: a symlink POINTING AT a
// genuine regular file is allowed — env.fileMode wraps os.Stat (follows
// symlinks), not os.Lstat, so this is an intentional, documented exception
// to "reject anything that isn't a plain regular file": the CONTENT behind
// the symlink is a real regular file, never read here, only passed through
// as a path.
func TestGogSetup_R205_SymlinkToRegularFileAllowed(t *testing.T) {
	gogSetupTestCfg(t)
	dir := t.TempDir()
	real := filepath.Join(dir, "real-client.json")
	if err := os.WriteFile(real, []byte(`{"installed":{"client_id":"fake"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "client.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	env := gogSetupRealFileModeEnv(t)
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: link}
	if err := gogSetup(env, opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
}

// TestGogSetup_R205_SymlinkToFIFORejected: a symlink whose TARGET is a FIFO
// (not a regular file) must be rejected exactly like the FIFO itself —
// os.Stat follows the link and reports the FIFO's mode.
func TestGogSetup_R205_SymlinkToFIFORejected(t *testing.T) {
	gogSetupTestCfg(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "real-fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}
	link := filepath.Join(dir, "client.json")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	env := gogSetupRealFileModeEnv(t)
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: link}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the credentials path is a symlink to a FIFO")
	}
	if !strings.Contains(err.Error(), "regular file") {
		t.Errorf("expected the regular-file guidance, got %q", err)
	}
}
