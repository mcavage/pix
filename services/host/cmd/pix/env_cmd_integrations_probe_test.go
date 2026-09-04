package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
)

// env_cmd_integrations_probe_test.go proves the OTHER half of the
// declared/registered/reachable answer end to end: a pix.toml
// `[host.mcp.<name>].probe_args` actually EXECUTES through `pix env show`,
// not merely through workflow/env's own unit tests. HostMCPFact's doc
// comment always promised "pix doctor runs it" (bom.go); this is the first
// real caller that does, reached through `pix env show`, the command a user
// actually types.
func TestEnvShow_DeclaredProbeActuallyRuns(t *testing.T) {
	home := envCmdTestHome(t)
	envRoot := filepath.Join(home, "envs", "work")
	if err := os.MkdirAll(envRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	scriptDir := t.TempDir()
	okScript := filepath.Join(scriptDir, "warehouse-ok")
	failScript := filepath.Join(scriptDir, "warehouse-fail")
	if err := os.WriteFile(okScript, []byte("#!/bin/sh\necho authenticated\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failScript, []byte("#!/bin/sh\necho not authenticated >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	sbxenv := fmt.Sprintf(`schemaVersion: "1"
agent: pix
mcp:
  servers:
    - name: warehouse-ok
      command: %s
    - name: warehouse-fail
      command: %s
`, okScript, failScript)
	if err := os.WriteFile(filepath.Join(envRoot, ".sbxenv.yaml"), []byte(sbxenv), 0o644); err != nil {
		t.Fatal(err)
	}
	pixToml := fmt.Sprintf(`schema = 1

[host.mcp.warehouse-ok]
probe_args = [%q, "probe"]

[host.mcp.warehouse-fail]
probe_args = [%q, "probe"]
`, okScript, failScript)
	if err := os.WriteFile(filepath.Join(envRoot, "pix.toml"), []byte(pixToml), 0o644); err != nil {
		t.Fatal(err)
	}

	installFakeSbxMcpLs(t) // "notion" only; neither warehouse-* name is registered
	t.Setenv("PIX_HOME", home)

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if code := dispatch([]string{"env", "show", "work"}, d); code != 0 {
		t.Fatalf("dispatch exit = %d, stderr=%s", code, errb.String())
	}
	got := out.String()

	if !strings.Contains(got, "warehouse-ok") || !strings.Contains(got, "registered:absent") {
		t.Errorf("warehouse-ok should be declared and registered:absent:\n%s", got)
	}
	// The whole point of this test: a REAL probe ran and its exit code
	// drove the verdict — never a guess from registration alone.
	okLine := lineContaining(got, "warehouse-ok ")
	if !strings.Contains(okLine, "reachable:ready") {
		t.Fatalf("warehouse-ok line = %q, want reachable:ready (its probe exits 0)", okLine)
	}
	failLine := lineContaining(got, "warehouse-fail ")
	if !strings.Contains(failLine, "reachable:absent") {
		t.Fatalf("warehouse-fail line = %q, want reachable:absent (its probe exits 1, a verified negative)", failLine)
	}
}

func lineContaining(s, substr string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, substr) {
			return line
		}
	}
	return ""
}
