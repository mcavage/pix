package env

// testhelpers_test.go — shared fixtures for tests that load a real
// *Environment (bom_test.go, bom_e18block_test.go, loadhome_test.go): the
// v2 pixhome-based equivalent of what the deleted v1 registry test helpers
// (tempConfig/loadConfig/Register + Load) used to provide.

import (
	"os"
	"path/filepath"
	"testing"

	"pix/host/pixhome"
)

// minimalSbxenv is the smallest valid native document: no host-executing
// facet at all, so an environment loaded from it alone is Tier0.
const minimalSbxenv = "schemaVersion: \"1\"\nagent: pix\n"

// minimalSidecarWithSkills is a pix.toml declaring one local [pi].skills
// entry at dir.
func minimalSidecarWithSkills(dir string) string {
	return "schema = 1\n\n[pi]\nskills = [\"" + dir + "\"]\n"
}

// writeEnvFile writes name (".sbxenv.yaml" or "pix.toml") under root.
func writeEnvFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// loadTestEnv creates a fresh PIX_HOME in t.TempDir(), writes sbxenv (and,
// when non-empty, sidecar) under envs/<name>/, and returns the LoadHome
// result — the v2 replacement for the deleted v1 helpers that registered
// a *config.Config entry and called Load.
func loadTestEnv(t *testing.T, name, sbxenv, sidecar string) *Environment {
	t.Helper()
	home := pixhome.New(t.TempDir())
	root := home.EnvironmentDir(name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", sbxenv)
	if sidecar != "" {
		writeEnvFile(t, root, "pix.toml", sidecar)
	}
	sel, err := ResolveIn(home, name)
	if err != nil {
		t.Fatalf("ResolveIn: %v", err)
	}
	env, err := LoadHome(sel, nil, noBareLookPath)
	if err != nil {
		t.Fatalf("LoadHome: %v", err)
	}
	return env
}
