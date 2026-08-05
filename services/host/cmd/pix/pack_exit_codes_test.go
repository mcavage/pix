package main

// pack_exit_codes_test.go pins the `pix pack` process contract against the
// REAL compiled binary.
//
// U08d moved the exit out of the pack capability: its verbs now return typed
// errors and packRun (pack_cmd.go) maps them to codes. That is a refactor with
// a user-visible surface — the stream a message lands on and the code the shell
// sees — so it is proven where the user meets it, not at the seam that changed.
// Everything below was captured from the binary BEFORE the refactor and must
// keep matching byte for byte:
//
//	pack ls (no pack)      exit 0, the "no active pack" line on STDOUT
//	pack use <not a pack>  exit 1, "pix pack use: ..." on STDOUT, stderr empty
//	pack use <Tier-1>      exit 1, BoM + refusal on STDOUT, nothing committed
//
// The bad-invocation (usage error, exit 2, bare stderr) case pack add <bad
// name> pinned is gone with `pack add` itself (U08f): kong's own grammar now
// requires `pack use`'s target, so there is no more pack-specific business-
// level usage error reachable from the real binary. The generic UsageError
// contract (retired_dispatch_test.go, root_test.go) still covers the shape.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runPackBinary runs the real binary with a throwaway HOME/config/state, a pipe
// stdin (deterministically non-TTY — the CI/script shape the trust gate's
// fail-closed contract is about), and returns stdout, stderr and the exit code.
func runPackBinary(t *testing.T, home string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command(buildPixBinary(t), args...)
	cmd.Stdin = strings.NewReader("")
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"PIX_CONFIG="+filepath.Join(home, "config.toml"),
		"XDG_STATE_HOME="+filepath.Join(home, "state"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"),
	)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code = 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("running pix %s: %v", strings.Join(args, " "), err)
	}
	return out.String(), errb.String(), code
}

func TestPackBinary_SuccessWritesStdoutAndExitsZero(t *testing.T) {
	stdout, stderr, code := runPackBinary(t, t.TempDir(), "pack", "ls")
	if code != 0 {
		t.Errorf("pack ls with no active pack must exit 0, got %d (stderr: %s)", code, stderr)
	}
	const want = "no active pack (create a pack.toml + skills/ by hand, then `pix pack use <path|git-url>`)\n"
	if stdout != want {
		t.Errorf("stdout mismatch:\n got: %q\nwant: %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("a successful verb writes nothing to stderr, got: %q", stderr)
	}
}

// TestPackBinary_OperationErrorIsExit1OnStdout: a failed operation reports on
// the verb's OWN stream (where the rest of its report went), prefixed with the
// verb, and exits 1.
func TestPackBinary_OperationErrorIsExit1OnStdout(t *testing.T) {
	home := t.TempDir()
	missing := filepath.Join(home, "nope")
	stdout, stderr, code := runPackBinary(t, home, "pack", "use", missing)
	if code != 1 {
		t.Errorf("adopting a directory that is not a pack must exit 1, got %d", code)
	}
	want := "pix pack use: " + missing + " is not a pack (no pack.toml)\n"
	if stdout != want {
		t.Errorf("stdout mismatch:\n got: %q\nwant: %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("an operation error is not also a usage error; stderr must be empty, got: %q", stderr)
	}
}

// TestPackBinary_Tier1NonTTYFailsClosed is the safety invariant, end to end
// through the shipped binary: a pack that would run host code refuses to adopt
// non-interactively, prints the bill of materials it refused, names --yes, and
// commits NOTHING. The capability-level twin (workflow/pack) asserts the same
// refusal as a returned error; this asserts the user-visible half.
func TestPackBinary_Tier1NonTTYFailsClosed(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, "pack")
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "platformio"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "name = \"work\"\nschema = 1\n\n[[proxy]]\nname = \"platformio\"\nhost = true\n"
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runPackBinary(t, home, "pack", "use", root)
	if code != 1 {
		t.Fatalf("a Tier-1 pack must fail closed on a non-TTY without --yes, got exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"This pack adds these integrations to Pix:",
		"Host wrapper:        platformio",
		"refusing to adopt it non-interactively (fail closed)",
		"--yes",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the refusal must show %q, got:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		b, _ := os.ReadFile(filepath.Join(home, "config.toml"))
		t.Errorf("nothing may commit on a fail-closed refusal; config exists:\n%s", b)
	}
}
