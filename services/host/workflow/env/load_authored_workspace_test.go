package env

// load_authored_workspace_test.go — restriction 4 for the mounts the
// DOCUMENT declares, not only the ones a caller passes in.
//
// Until envinfo modeled sbx 0.39's `workspace:`/`additionalWorkspaces:`
// keys, an authored file could mount its own directory and Load would
// never know: containment only ever saw a caller-supplied EffectiveMounts,
// and cmd/pix passes nil. Upstream's own reference is explicit about why
// that matters ("Keep the environment file outside the directories you
// mount into the sandbox. That includes the primary workspace and every
// additionalWorkspaces mount") — the agent can otherwise rewrite the file
// that controls the next `sbx env` command.

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
)

// registerEnvWithSbxenv writes an environment file at a fresh root and
// registers it, returning the root.
func registerEnvWithSbxenv(t *testing.T, cfg *config.Config, root, name, body string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeEnvFile(t, root, ".sbxenv.yaml", body)
	if _, err := Register(cfg, name, root); err != nil {
		t.Fatal(err)
	}
}

// TestLoad_RefusesRootInsideAuthoredWorkspace is the core refusal: the
// document mounts the directory it lives in.
func TestLoad_RefusesRootInsideAuthoredWorkspace(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	root := t.TempDir()
	registerEnvWithSbxenv(t, cfg, root, "home", "schemaVersion: \"1\"\nagent: pix\nworkspace: .\n")

	_, err := Load(cfg, nil, "home", nil, nil)
	if err == nil {
		t.Fatal("Load accepted an environment whose own file mounts its own directory writable")
	}
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("cli.ExitCode(err) = %d, want 2", got)
	}
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError", err)
	}
	if !containsAll(err.Error(), hosttrust.CanonicalRoot(root)) {
		t.Errorf("refusal text %q must name the root", err.Error())
	}
}

// TestLoad_RefusesRootInsideAuthoredAdditionalWorkspace covers the other
// authored key. `..` is the realistic shape: an environment directory
// beside the project, mounting the parent of both.
func TestLoad_RefusesRootInsideAuthoredAdditionalWorkspace(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "env")
	registerEnvWithSbxenv(t, cfg, root, "home", "schemaVersion: \"1\"\nagent: pix\nadditionalWorkspaces:\n  - path: ..\n")

	_, err := Load(cfg, nil, "home", nil, nil)
	var containment *ContainmentError
	if !errors.As(err, &containment) {
		t.Fatalf("error = %#v, want *ContainmentError naming %s", err, parent)
	}
}

// TestLoad_AcceptsCloneAndReadOnlyAuthoredWorkspaces pins the two cases
// that must NOT refuse: restriction 4 is a WRITABLE-workspace rule. Clone
// mode bind-mounts the host tree read-only, and an authored `readOnly:
// true` additional mount says so outright.
func TestLoad_AcceptsCloneAndReadOnlyAuthoredWorkspaces(t *testing.T) {
	tempConfig(t)
	cfg := loadConfig(t)
	for _, tc := range []struct{ name, body string }{
		{"clone-primary", "schemaVersion: \"1\"\nagent: pix\nworkspace:\n  path: .\n  clone: true\n"},
		{"readonly-additional", "schemaVersion: \"1\"\nagent: pix\nadditionalWorkspaces:\n  - path: .\n    readOnly: true\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			registerEnvWithSbxenv(t, cfg, root, "home-"+tc.name, tc.body)
			if _, err := Load(cfg, nil, "home-"+tc.name, nil, nil); err != nil {
				t.Fatalf("Load refused a non-writable authored workspace: %v", err)
			}
		})
	}
}

// TestComputeBoM_IncludesAuthoredAdditionalWorkspaces proves a reviewer is
// shown (and the acceptance fingerprint covers) the mounts the document
// itself declares — the pairing AC-66 requires, now that those mounts are
// parseable at all. The authored PRIMARY workspace is deliberately absent:
// a launch overrides it with the run's own project workspace, so listing
// it would ask for consent to access Pix never grants.
func TestComputeBoM_IncludesAuthoredAdditionalWorkspaces(t *testing.T) {
	tempConfig(t)
	parent := t.TempDir()
	root := filepath.Join(parent, "env")
	shared := filepath.Join(parent, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := loadConfig(t)
	registerEnvWithSbxenv(t, cfg, root, "home",
		"schemaVersion: \"1\"\nagent: pix\nworkspace: ../project\nadditionalWorkspaces:\n  - path: ../shared\n    readOnly: true\n")

	loaded, err := Load(cfg, nil, "home", nil, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	bom, err := ComputeBoM(loaded, nil, nil)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}
	if len(bom.EffectiveMounts) != 1 {
		t.Fatalf("EffectiveMounts = %+v, want exactly the authored additional workspace", bom.EffectiveMounts)
	}
	got := bom.EffectiveMounts[0]
	if got.Path != shared || !got.ReadOnly {
		t.Errorf("EffectiveMounts[0] = %+v, want {%s true}", got, shared)
	}
	if !bom.Tier1() {
		t.Errorf("a declared mount must make the bill reviewable (mounts expand host access)")
	}
}
