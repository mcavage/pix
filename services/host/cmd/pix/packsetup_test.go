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
	state := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(state, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(state, "state"))
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
	p, err := loadPack(root)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := computeHostExecFingerprint(root, computeHostBoM(p, "", func(string) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	store := &packTrustStore{Version: 1}
	store.recordAcceptance(store.trustKey(root), packTrustRecord{Path: canonicalizePackRoot(root), Fingerprint: fp})
	if err := store.save(); err != nil {
		t.Fatal(err)
	}
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
			if filepath.Base(name) != "account" || strings.Join(args, " ") != "apply" {
				t.Fatalf("unexpected apply: %s %v", name, args)
			}
			ready = true
			return nil
		},
	}
	var out bytes.Buffer
	if err := runPackSetup(env, &out, root, nil, true); err != nil {
		t.Fatal(err)
	}
	if checks != 2 || applies != 1 {
		t.Fatalf("checks=%d applies=%d, want 2 and 1", checks, applies)
	}
	if !strings.Contains(out.String(), "verified") {
		t.Fatalf("missing post-apply verdict: %s", out.String())
	}
}

func TestRunPackSetupRejectsHookChangedAfterAcceptance(t *testing.T) {
	root := writeSetupPack(t, true)
	hook := filepath.Join(root, "setup", "account")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho attacker\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	probes, applies := 0, 0
	env := shellEnv{
		probe:          func(string, ...string) (string, bool, error) { probes++; return "", false, nil },
		runInteractive: func(string, ...string) error { applies++; return nil },
	}
	err := runPackSetup(env, &bytes.Buffer{}, root, nil, true)
	if err == nil || !strings.Contains(err.Error(), "changed since acceptance") {
		t.Fatalf("mutated hook error = %v", err)
	}
	if probes != 0 || applies != 0 {
		t.Fatalf("mutated hook executed: probes=%d applies=%d", probes, applies)
	}
}

func TestRunPackSetupExecutesSnapshotWhenSourceChangesAfterCheck(t *testing.T) {
	root := writeSetupPack(t, true)
	hook := filepath.Join(root, "setup", "account")
	ready := false
	checks := 0
	env := shellEnv{
		probe: func(name string, args ...string) (string, bool, error) {
			checks++
			if checks == 1 {
				if err := os.WriteFile(hook, []byte("#!/bin/sh\necho attacker\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				return "", false, fmt.Errorf("not ready")
			}
			if ready {
				return "", false, nil
			}
			return "", false, fmt.Errorf("not ready")
		},
		runInteractive: func(name string, args ...string) error {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			if strings.Contains(string(data), "attacker") || !strings.Contains(string(data), "exit 0") {
				return fmt.Errorf("apply did not execute accepted snapshot: %q", data)
			}
			ready = true
			return nil
		},
	}
	if err := runPackSetup(env, &bytes.Buffer{}, root, nil, true); err != nil {
		t.Fatal(err)
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
	if err := runPackSetup(env, &bytes.Buffer{}, root, nil, true); err != nil {
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
	if err := runPackSetup(env, &bytes.Buffer{}, root, []string{"account"}, true); err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("requested optional setup hook was not run")
	}
	if err := runPackSetup(env, &bytes.Buffer{}, root, []string{"missing"}, true); err == nil {
		t.Fatal("unknown requested setup id must fail")
	}
}

func TestRunPackSetupRejectsUnknownBeforeAnyHook(t *testing.T) {
	root := writeSetupPack(t, true)
	probes, applies := 0, 0
	env := shellEnv{
		probe:          func(string, ...string) (string, bool, error) { probes++; return "", false, nil },
		runInteractive: func(string, ...string) error { applies++; return nil },
	}
	if err := runPackSetup(env, &bytes.Buffer{}, root, []string{"typo"}, true); err == nil {
		t.Fatal("unknown hook should fail")
	}
	if probes != 0 || applies != 0 {
		t.Fatalf("unknown hook caused side effects: probes=%d applies=%d", probes, applies)
	}
}

func TestRunPackSetupNonInteractiveNeverAppliesUnreadyHook(t *testing.T) {
	root := writeSetupPack(t, true)
	applies := 0
	env := shellEnv{
		probe:          func(string, ...string) (string, bool, error) { return "", false, fmt.Errorf("not ready") },
		runInteractive: func(string, ...string) error { applies++; return nil },
	}
	err := runPackSetup(env, &bytes.Buffer{}, root, nil, false)
	if err == nil || !strings.Contains(err.Error(), "interactive authorization") {
		t.Fatalf("error = %v", err)
	}
	if applies != 0 {
		t.Fatal("non-interactive setup must never execute an unready browser-capable hook")
	}
}

func TestPlanPackSetupRequestsAssignsHookToOwningPack(t *testing.T) {
	first := writeSetupPack(t, true)
	second := t.TempDir()
	if err := os.MkdirAll(filepath.Join(second, "setup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "setup", "extra"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, second, packManifest{Name: "second", Schema: 1, Setup: []packSetupStep{{
		ID: "extra", Path: "setup/extra", CheckArgs: []string{"check"}, ApplyArgs: []string{"apply"},
	}}})
	p, err := loadPack(second)
	if err != nil {
		t.Fatal(err)
	}
	hookBytes, err := os.ReadFile(filepath.Join(second, "setup", "extra"))
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := computeHostExecFingerprintWithSetup(second, computeHostBoM(p, "", func(string) bool { return false }), map[string][]byte{"extra": hookBytes})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutatePackTrustStore(func(store *packTrustStore) error {
		store.recordAcceptance(store.trustKey(second), packTrustRecord{
			Path: canonicalizePackRoot(second), Fingerprint: fp,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := planPackSetupRequests([]string{first, second}, []string{"extra"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan[first]) != 0 || strings.Join(plan[second], ",") != "extra" {
		t.Fatalf("plan = %#v", plan)
	}
}

func TestPlanPackSetupRequestsRejectsAmbiguousHookBeforeExecution(t *testing.T) {
	first := writeSetupPack(t, false)
	repeated, err := planPackSetupRequests([]string{first, first}, []string{"account"})
	if err != nil || len(repeated[first]) != 1 {
		t.Fatalf("repeating one pack must not create ambiguous ownership: plan=%v error=%v", repeated, err)
	}
	second := writeSetupPack(t, false)
	plan, err := planPackSetupRequests([]string{first, second}, []string{"account"})
	if err == nil || !strings.Contains(err.Error(), "multiple active packs") ||
		!strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
		t.Fatalf("plan=%v error=%v", plan, err)
	}
	if plan != nil {
		t.Fatalf("ambiguous plan must be discarded before any hook runs: %#v", plan)
	}
}

func TestPackSetupPlanRunsOptionalHookOnlyForItsOwner(t *testing.T) {
	first := t.TempDir()
	if err := os.MkdirAll(filepath.Join(first, "setup"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(first, "setup", "ignored"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWritePack(t, first, packManifest{Name: "first", Schema: 1, Setup: []packSetupStep{{
		ID: "ignored", Path: "setup/ignored", CheckArgs: []string{"check"}, ApplyArgs: []string{"apply"},
	}}})
	second := writeSetupPack(t, false)
	p, err := loadPack(first)
	if err != nil {
		t.Fatal(err)
	}
	fp, _, err := computeHostExecFingerprint(first, computeHostBoM(p, "", func(string) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mutatePackTrustStore(func(store *packTrustStore) error {
		store.recordAcceptance(store.trustKey(first), packTrustRecord{
			Path: canonicalizePackRoot(first), Fingerprint: fp,
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := planPackSetupRequests([]string{first, second}, []string{"account"})
	if err != nil {
		t.Fatal(err)
	}
	ready := map[string]bool{}
	var applied []string
	env := shellEnv{
		probe: func(name string, _ ...string) (string, bool, error) {
			if ready[name] {
				return "", false, nil
			}
			return "", false, fmt.Errorf("not ready")
		},
		runInteractive: func(name string, _ ...string) error {
			applied = append(applied, name)
			ready[name] = true
			return nil
		},
	}
	var out bytes.Buffer
	for _, root := range []string{first, second} {
		if err := runPackSetup(env, &out, root, plan[root], true); err != nil {
			t.Fatal(err)
		}
	}
	if len(applied) != 1 || !strings.HasSuffix(applied[0], "account") {
		t.Fatalf("applied hooks = %v; transcript:\n%s", applied, out.String())
	}
	if strings.Contains(out.String(), "ignored") || !strings.Contains(out.String(), "Connect account") {
		t.Fatalf("setup transcript did not reflect owner routing:\n%s", out.String())
	}
}

func TestNormalizeSetupPackArg(t *testing.T) {
	if got := normalizeSetupPackArg("acme/work-pack"); got != "https://github.com/acme/work-pack.git" {
		t.Fatalf("got %q", got)
	}
	for _, unchanged := range []string{"./local", "/tmp/local", "https://github.com/a/b.git", "git@github.com:a/b.git"} {
		if got := normalizeSetupPackArg(unchanged); got != unchanged {
			t.Fatalf("normalize %q = %q", unchanged, got)
		}
	}
}
