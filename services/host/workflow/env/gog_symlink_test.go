// gog_symlink_test.go proves the actual reported blocker is fixed:
// `/opt/homebrew/bin/gog` (or any Homebrew-installed command) is an
// ordinary symlink into its own Cellar keg, so an environment naming it
// as an `mcp.servers[].command` used to make `pix env show`/`--effective`
// refuse to load at all (RefuseSymlinkedReference's old "any symlink is
// refused" rule). These tests exercise the real end-to-end path
// (LoadHome, ComputeBoM) with a RELATIVE command so they never depend on
// $PATH or what happens to be installed on the machine running them.
package env

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/pixhome"
)

// gogFixtureRoot builds an environment root containing a relative MCP
// server command ("./bin/gog") that is a symlink to a real, executable
// target ("./Cellar/gog/1.0/bin/gog") — the exact shape a Homebrew
// installation produces. sbxenv is the caller-supplied document text so a
// broken-link variant can reuse this scaffolding.
func gogFixtureRoot(t *testing.T, sbxenv string) (pixhome.Paths, string) {
	t.Helper()
	home := pixhome.New(t.TempDir())
	root := home.EnvironmentDir("work")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", sbxenv)
	return home, root
}

const gogSbxenv = "schemaVersion: \"1\"\nagent: pix\n\nmcp:\n  servers:\n    - name: gog\n      command: ./bin/gog\n"

// TestLoadHome_MCPServerSymlinkedCommandResolves is `pix env show work`'s
// (and `--effective`'s) LoadHome half of the reported blocker: an
// ordinary symlink to a real executable no longer refuses the load.
func TestLoadHome_MCPServerSymlinkedCommandResolves(t *testing.T) {
	home, root := gogFixtureRoot(t, gogSbxenv)

	realExec := filepath.Join(root, "Cellar", "gog", "1.0", "bin", "gog")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realExec, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "gog")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realExec, link); err != nil {
		t.Fatal(err)
	}

	sel, err := ResolveIn(home, "work")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	if _, err := LoadHome(sel, nil, noBareLookPath); err != nil {
		t.Fatalf("LoadHome must resolve a Homebrew-style symlinked MCP server command instead of refusing: %v", err)
	}
}

// TestLoadHome_MCPServerBrokenSymlinkedCommandRefuses is the negative
// control: a symlink that does not resolve to anything is still refused,
// proving the fix did not simply stop checking.
func TestLoadHome_MCPServerBrokenSymlinkedCommandRefuses(t *testing.T) {
	home, root := gogFixtureRoot(t, gogSbxenv)

	link := filepath.Join(root, "bin", "gog")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "Cellar", "gog", "1.0", "bin", "gog"), link); err != nil {
		t.Fatal(err)
	}

	sel, err := ResolveIn(home, "work")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	_, err = LoadHome(sel, nil, noBareLookPath)
	if err == nil {
		t.Fatal("LoadHome must still refuse a broken symlinked MCP server command")
	}
	var refErr *ReferenceTargetError
	if !errors.As(err, &refErr) || refErr.Reason != ReferenceBroken {
		t.Fatalf("refusal = %#v, want ReferenceTargetError{Reason: broken}", err)
	}
}

// TestComputeBoM_MCPServerSymlinkedCommandFingerprintsResolvedTarget is
// the trust-bill half: the resolved PHYSICAL target is what gets
// fingerprinted and is available for renderTrustBill to show alongside
// the authored command, never the unresolved symlink path.
func TestComputeBoM_MCPServerSymlinkedCommandFingerprintsResolvedTarget(t *testing.T) {
	home, root := gogFixtureRoot(t, gogSbxenv)

	realExec := filepath.Join(root, "Cellar", "gog", "1.0", "bin", "gog")
	if err := writeFile(t, realExec, "#!/bin/sh\n"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realExec, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "gog")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realExec, link); err != nil {
		t.Fatal(err)
	}

	sel, err := ResolveIn(home, "work")
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	env, err := LoadHome(sel, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("LoadHome: %v", err)
	}
	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}

	if len(bom.MCPServers) != 1 {
		t.Fatalf("MCPServers = %+v, want exactly one entry", bom.MCPServers)
	}
	got := bom.MCPServers[0]
	if got.Command != "./bin/gog" {
		t.Errorf("Command = %q, want the AUTHORED reference %q unchanged", got.Command, "./bin/gog")
	}
	wantTarget, err := filepath.EvalSymlinks(realExec)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target != wantTarget {
		t.Errorf("Target = %q, want the resolved physical target %q", got.Target, wantTarget)
	}
	if got.SHA == "" {
		t.Error("SHA must be populated from the resolved target's content, not empty")
	}

	// Fingerprint must succeed (never refuse) and must actually depend on
	// the resolved content: changing the real executable's bytes changes
	// the fingerprint, proving Target/SHA are truly part of what a human
	// approves with `pix env trust`, not decorative fields nothing reads.
	fp1, err := Fingerprint(bom)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}
	if err := os.WriteFile(realExec, []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bom2, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("ComputeBoM (after content change): %v", err)
	}
	fp2, err := Fingerprint(bom2)
	if err != nil {
		t.Fatalf("Fingerprint (after content change): %v", err)
	}
	if fp1 == fp2 {
		t.Error("Fingerprint must change when the resolved target's content changes")
	}
}
