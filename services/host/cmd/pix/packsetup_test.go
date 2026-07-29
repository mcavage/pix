package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSetupPack(t *testing.T, required bool) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "setup"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(root, "setup", "account")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1, Setup: []packSetupStep{{
		ID: "account", Path: "setup/account", CheckArgs: []string{"check"},
		ApplyArgs: []string{"apply"}, Required: required, Description: "Connect account",
	}}})
	return root
}

func TestPackSetupValidationRejectsUnsafeOrNonExecutableHooks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hook"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, step := range []packSetupStep{
		{ID: "../bad", Path: "hook"},
		{ID: "good", Path: "../escape"},
		{ID: "good", Path: "hook"},
	} {
		m := packManifest{Name: "x", Schema: 1, Setup: []packSetupStep{step}}
		if err := validatePackFacets(root, &m); err == nil {
			t.Fatalf("expected validation failure for %+v", step)
		}
	}
}

func TestPackSetupValidationRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "hook"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "setup")); err != nil {
		t.Fatal(err)
	}
	m := packManifest{Name: "x", Schema: 1, Setup: []packSetupStep{{ID: "bad", Path: "setup/hook"}}}
	if err := validatePackFacets(root, &m); err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("got %v, want intermediate symlink rejection", err)
	}
}

func TestPackIntegrationSetupMustReferenceDeclaredHook(t *testing.T) {
	root := t.TempDir()
	m := packManifest{Name: "x", Schema: 1, Integrations: []packIntegration{{Name: "CRM", MCP: "crm", Setup: "missing"}}}
	if err := validatePackFacets(root, &m); err == nil || !strings.Contains(err.Error(), "unknown setup hook") {
		t.Fatalf("got %v, want unknown setup hook failure", err)
	}
}

func TestPackSetupFingerprintChangesWithHookBytes(t *testing.T) {
	root := writeSetupPack(t, true)
	p, err := loadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	bom := computeHostBoM(p, "", func(string) bool { return false })
	first, _, err := computeHostExecFingerprint(root, bom)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "setup", "account"), []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	second, _, err := computeHostExecFingerprint(root, bom)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("changing setup hook bytes must change the host-exec fingerprint")
	}
}

func TestRunRequiredPackSetupProbesAppliesAndReprobes(t *testing.T) {
	root := writeSetupPack(t, true)
	ready := false
	checks := 0
	applies := 0
	env := shellEnv{
		probe: func(name string, args ...string) (string, bool, error) {
			checks++
			if ready {
				return "", false, nil
			}
			return "", false, fmt.Errorf("not ready")
		},
		runInteractive: func(name string, args ...string) error {
			applies++
			if filepath.Clean(name) != filepath.Join(root, "setup", "account") || strings.Join(args, " ") != "apply" {
				t.Fatalf("unexpected apply: %s %v", name, args)
			}
			ready = true
			return nil
		},
	}
	var out bytes.Buffer
	if err := runPackSetup(env, &out, root, nil); err != nil {
		t.Fatal(err)
	}
	if checks != 2 || applies != 1 {
		t.Fatalf("checks=%d applies=%d, want 2 and 1", checks, applies)
	}
	if !strings.Contains(out.String(), "verified") {
		t.Fatalf("missing post-apply verdict: %s", out.String())
	}
}

func TestRunRequiredPackSetupSkipsOptionalSteps(t *testing.T) {
	root := writeSetupPack(t, false)
	env := shellEnv{
		probe: func(string, ...string) (string, bool, error) {
			t.Fatal("optional hook was probed")
			return "", false, nil
		},
		runInteractive: func(string, ...string) error { t.Fatal("optional hook was run"); return nil },
	}
	if err := runPackSetup(env, &bytes.Buffer{}, root, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRunPackSetupRunsRequestedOptionalStep(t *testing.T) {
	root := writeSetupPack(t, false)
	ready := false
	env := shellEnv{
		probe: func(string, ...string) (string, bool, error) {
			if ready {
				return "", false, nil
			}
			return "", false, fmt.Errorf("not ready")
		},
		runInteractive: func(string, ...string) error { ready = true; return nil },
	}
	if err := runPackSetup(env, &bytes.Buffer{}, root, []string{"account"}); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("requested optional setup hook was not run")
	}
	if err := runPackSetup(env, &bytes.Buffer{}, root, []string{"missing"}); err == nil {
		t.Fatal("unknown requested setup id must fail")
	}
}

func TestNormalizeSetupPackArg(t *testing.T) {
	if got := normalizeSetupPackArg("docker/gm-pix-pack"); got != "https://github.com/docker/gm-pix-pack.git" {
		t.Fatalf("got %q", got)
	}
	for _, unchanged := range []string{"./local", "/tmp/local", "https://github.com/a/b.git", "git@github.com:a/b.git"} {
		if got := normalizeSetupPackArg(unchanged); got != unchanged {
			t.Fatalf("normalize %q = %q", unchanged, got)
		}
	}
}
