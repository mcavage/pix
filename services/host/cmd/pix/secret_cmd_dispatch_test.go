// secret_cmd_dispatch_test.go — argv dispatch and exit codes for `pix secret`.
// The subject is SecretCmd (the kong tree in cmd/pix), not the secret
// capability, so these live here; the capability's own behaviour is tested in
// secret/.
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// runSecretDispatch drives `pix secret ...` through the REAL root and exits
// with the code the launcher would. The command returns its error now rather
// than calling os.Exit, but these cases assert the PROCESS exit code, so the
// subprocess arm is where error becomes exit.
func runSecretDispatch(argv []string) {
	d := &cli.Deps{Out: os.Stdout, Err: os.Stderr, In: os.Stdin}
	os.Exit(dispatch(append([]string{"secret"}, argv...), d))
}

// TestSecretCheckRejectsTrailingArg covers F6: `secret check --bogus` exits 2.
// runSecretCmd calls os.Exit, so we exercise it in a subprocess.
func TestSecretCheckRejectsTrailingArg(t *testing.T) {
	if os.Getenv("PIX_SECRET_BOGUS") == "1" {
		runSecretDispatch([]string{"check", "--bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCheckRejectsTrailingArg")
	cmd.Env = append(os.Environ(), "PIX_SECRET_BOGUS=1")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v", err)
	}
	if ee.ExitCode() != 2 {
		t.Errorf("secret check --bogus exit code = %d, want 2", ee.ExitCode())
	}
}

// TestSecretCmdArgCounts covers the dispatch surface: `set` requires exactly 2
// args, `rm` requires exactly 1, and an unknown subcommand names the new CRUD
// surface. All run in a subprocess since runSecretCmd calls os.Exit.
func TestSecretCmdArgCounts(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"set too few", []string{"set", "ONLY_ONE"}},
		{"set too many", []string{"set", "A", "op://v/i/f", "extra"}},
		{"rm too many", []string{"rm", "A", "B"}},
		{"unknown", []string{"frobnicate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("PIX_SECRET_ARGCOUNT") == tc.name {
				runSecretDispatch(tc.argv)
				return
			}
			cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCmdArgCounts/"+strings.ReplaceAll(tc.name, " ", "_"))
			cmd.Env = append(os.Environ(), "PIX_SECRET_ARGCOUNT="+tc.name)
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("expected an ExitError, got %v", err)
			}
			if ee.ExitCode() != 2 {
				t.Errorf("exit code = %d, want 2", ee.ExitCode())
			}
		})
	}
}

func TestSecretHelpConfigIndependent(t *testing.T) {
	// -h prints usage and touches NOTHING: kong answers help before any leaf
	// runs, so no config read and no op call can happen on the way.
	d, out, _ := rootDeps()
	if err := runRootParse([]string{"secret", "--help"}, d); err != nil {
		t.Fatalf("secret --help: %v", err)
	}
	if !strings.Contains(out.String(), "Usage: pix secret") {
		t.Errorf("secret --help must render the generated usage, got:\n%s", out.String())
	}
}

// TestSecretSetMirrorFailure_DispatcherExitsNonzero: a hostmode.env mirror
// failure for a provider key must make the CLI exit nonzero, never quietly
// succeed. Runs through the REAL dispatcher (the kong root) against the real
// filesystem in a subprocess, so the exit code is genuinely observed rather
// than merely a returned error value nobody acted on.
func TestSecretSetMirrorFailure_DispatcherExitsNonzero(t *testing.T) {
	if cfgDir := os.Getenv("PIX_SECRET_SET_MIRROR_FAIL_CFGDIR"); cfgDir != "" {
		runSecretDispatch([]string{"set", "ANTHROPIC_API_KEY", "op://v/anthropic/key"})
		return
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	// Sabotage hostmode.env: pre-create it as a DIRECTORY, so the atomic
	// rename-into-place mirror write fails (EISDIR), while op-refs.env (which
	// doesn't exist yet, and gets freshly seeded) stays a normal writable file.
	if err := os.MkdirAll(filepath.Join(cfgDir, "hostmode.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetMirrorFailure_DispatcherExitsNonzero")
	cmd.Env = append(os.Environ(),
		"PIX_SECRET_SET_MIRROR_FAIL_CFGDIR="+cfgDir,
		"PIX_CONFIG="+filepath.Join(cfgDir, "config.toml"),
	)
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError (a mirror failure must exit nonzero), got %v (Output: %s)", err, outBuf)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("exit code = 0, want nonzero, Output: %s", outBuf)
	}
	if !strings.Contains(string(outBuf), "could not mirror") {
		t.Errorf("output should explain the mirror failure, got:\n%s", outBuf)
	}
}

// TestSecretRm_DispatcherExitsNonzeroOnPartialFailure exercises the same
// partial failure through the real dispatcher, proving the CLI itself exits
// nonzero (not merely that secret.RunSecretRm returns an error nobody consumed).
func TestSecretRm_DispatcherExitsNonzeroOnPartialFailure(t *testing.T) {
	if cfgDir := os.Getenv("PIX_SECRET_RM_PARTIAL_FAIL_CFGDIR"); cfgDir != "" {
		runSecretDispatch([]string{"rm", "ANTHROPIC_API_KEY"})
		return
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "op-refs.env"), []byte("ANTHROPIC_API_KEY=op://v/anthropic/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// hostmode.env exists with the same key, but AS A DIRECTORY so the rename-in
	// removal write fails (EISDIR).
	if err := os.MkdirAll(filepath.Join(cfgDir, "hostmode.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretRm_DispatcherExitsNonzeroOnPartialFailure")
	cmd.Env = append(os.Environ(),
		"PIX_SECRET_RM_PARTIAL_FAIL_CFGDIR="+cfgDir,
		"PIX_CONFIG="+filepath.Join(cfgDir, "config.toml"),
	)
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError (a partial rm failure must exit nonzero), got %v (Output: %s)", err, outBuf)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("exit code = 0, want nonzero, Output: %s", outBuf)
	}
}
