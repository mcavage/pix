// Round-4 review fixes (packs-v2 Phase 1):
//
//	F1 — lock-write failure ABORTS before cfg.Save (config is never committed
//	     without its attribution), in both `pack use` and the active-pack
//	     `pack add mcp` path.
//	F2 — kit synthesis fails CLOSED at the launch boundary: a pack declaring a
//	     sandbox proxy whose kit can't be built refuses the launch instead of
//	     creating a sandbox missing the declared wrapper.
//	F3 — definitelyCreating no longer counts --replace on an INCONCLUSIVE
//	     probe (sbxUnknown) as a definite create (covered by the updated table
//	     in TestSandboxPackMarker_NotOverwrittenOnInconclusiveProbe).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// brokenPackLock makes writePackLock(root, ...) fail deterministically: the
// destination pack.lock is a non-empty DIRECTORY, so the atomic tmp+rename in
// writePackLock fails (rename onto a directory), while everything else in the
// pack root (pack.toml, bin/) stays perfectly readable/writable.
func brokenPackLock(t *testing.T, root string) {
	t.Helper()
	lockDir := packLockPath(root)
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lockDir, "occupied"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- F1: abort-on-lock-failure, unit level -------------------------------------

// TestCommitPackActivation_LockFailureAbortsBeforeSave: when the lock can't be
// written, commitPackActivation returns an error WITHOUT calling cfg.Save —
// the on-disk config is untouched (here: never even created).
func TestCommitPackActivation_LockFailureAbortsBeforeSave(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", cfgPath)

	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "p", Schema: 1})
	brokenPackLock(t, root)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddMCP("fastmail")
	cfg.Pack = root

	if err := commitPackActivation(cfg, root, packLock{MCP: []string{"fastmail"}}); err == nil {
		t.Fatal("expected an error when the lock cannot be written")
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("F1: cfg.Save must not run after a lock-write failure; config file exists (stat err=%v)", err)
	}

	// Sanity: with a writable lock the same commit succeeds and saves.
	if err := os.RemoveAll(packLockPath(root)); err != nil {
		t.Fatal(err)
	}
	if err := commitPackActivation(cfg, root, packLock{MCP: []string{"fastmail"}}); err != nil {
		t.Fatalf("commit with a writable lock should succeed: %v", err)
	}
	if readPackLock(root).MCP[0] != "fastmail" {
		t.Error("lock not written on the success path")
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(saved.MCP, "fastmail") {
		t.Error("config not saved on the success path")
	}
}

// --- F1: abort-on-lock-failure, end-to-end through runPackUse ------------------

// TestPackUse_LockWriteFailureAbortsWithoutCommit re-executes the test binary
// (runPackUse calls os.Exit on this path) and asserts a forced writePackLock
// failure leaves the config UNCOMMITTED: no MCP added, pack not switched — the
// config file is never written at all.
func TestPackUse_LockWriteFailureAbortsWithoutCommit(t *testing.T) {
	if os.Getenv("PI_STACK_TEST_LOCKFAIL") == "use" {
		// Child: this os.Exits(1) at the commit point if the fix holds.
		runPackUse(fakeGitEnv(nil), os.Stdout, []string{os.Getenv("PI_STACK_TEST_PACK_ROOT")})
		return // reaching here (exit 0) means runPackUse did NOT abort
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1, Integrations: []packIntegration{
		{Name: "fastmail", MCP: "fastmail"},
	}})
	brokenPackLock(t, root)

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackUse_LockWriteFailureAbortsWithoutCommit$")
	cmd.Env = append(os.Environ(),
		"PI_STACK_TEST_LOCKFAIL=use",
		"PI_STACK_TEST_PACK_ROOT="+root,
		"PI_STACK_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected `pack use` to exit non-zero on a lock-write failure; output:\n%s", out)
	}
	if !strings.Contains(string(out), "aborting without saving config") {
		t.Errorf("expected the abort message, got:\n%s", out)
	}
	// Nothing committed: the config file must not exist (Save never ran), so
	// the pack was not switched and no MCP was added.
	if _, serr := os.Stat(cfgPath); !os.IsNotExist(serr) {
		b, _ := os.ReadFile(cfgPath)
		t.Errorf("F1: config must stay uncommitted after a lock failure; found:\n%s", b)
	}
}

// TestPackAddMcp_LockWriteFailureAbortsWithoutCommit: same guarantee on the
// active-pack `pack add mcp` path — the pre-existing config (active pack, no
// MCP) is left byte-for-byte alone when the lock can't be written.
func TestPackAddMcp_LockWriteFailureAbortsWithoutCommit(t *testing.T) {
	if os.Getenv("PI_STACK_TEST_LOCKFAIL") == "add" {
		runPackAdd(fakeGitEnv(nil), os.Stdout, []string{"mcp", "fastmail", os.Getenv("PI_STACK_TEST_PACK_ROOT")})
		return
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, packManifest{Name: "work", Schema: 1})
	brokenPackLock(t, root)
	// The pack is ACTIVE (that is what routes `pack add mcp` into the attach+
	// commit path).
	before := "pack = \"" + root + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackAddMcp_LockWriteFailureAbortsWithoutCommit$")
	cmd.Env = append(os.Environ(),
		"PI_STACK_TEST_LOCKFAIL=add",
		"PI_STACK_TEST_PACK_ROOT="+root,
		"PI_STACK_CONFIG="+cfgPath,
		"XDG_STATE_HOME="+filepath.Join(dir, "state"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected `pack add mcp` to exit non-zero on a lock-write failure; output:\n%s", out)
	}
	if !strings.Contains(string(out), "aborting without saving config") {
		t.Errorf("expected the abort message, got:\n%s", out)
	}
	after, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != before {
		t.Errorf("F1: config must be unchanged after a lock failure (no MCP committed).\nbefore: %q\nafter:  %q", before, after)
	}
}

// --- F2: launch fails closed on a declared-but-unbuildable proxy ---------------

// TestApplyPackToLaunch_FailsClosedOnBrokenDeclaredProxy: a pack that DECLARES
// a sandbox proxy whose wrapper can't be read makes applyPackToLaunch return an
// error (the launch path aborts), never a kitless create — while "no proxies
// declared" and "buildable proxy" both proceed.
func TestApplyPackToLaunch_FailsClosedOnBrokenDeclaredProxy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// 1) Declared proxy, no bin/<name> on disk: the launch must refuse.
	broken := filepath.Join(dir, "broken-pack")
	mustWritePack(t, broken, packManifest{Name: "broken", Schema: 1, Proxies: []packProxy{{Name: "snowflake"}}})
	cfg.Pack = broken
	o := runOpts{}
	if lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); lerr == nil {
		t.Fatal("F2: expected an error for a declared sandbox proxy whose kit can't be built")
	} else if !strings.Contains(lerr.Error(), "refusing") {
		t.Errorf("expected a refusal message, got: %v", lerr)
	}
	if len(o.PackKits) != 0 {
		t.Errorf("no kit may be stacked on a failed synth, got %v", o.PackKits)
	}

	// 2) No proxies declared: fine, no kit, no error.
	plain := filepath.Join(dir, "plain-pack")
	mustWritePack(t, plain, packManifest{Name: "plain", Schema: 1})
	cfg.Pack = plain
	o = runOpts{}
	if lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); lerr != nil {
		t.Fatalf("a pack with no proxies must launch fine: %v", lerr)
	}
	if len(o.PackKits) != 0 {
		t.Errorf("no kit expected for a proxy-less pack, got %v", o.PackKits)
	}

	// 3) Buildable proxy: kit stacked, no error.
	good := filepath.Join(dir, "good-pack")
	mustWritePack(t, good, packManifest{Name: "good", Schema: 1, Proxies: []packProxy{{Name: "snowflake"}}})
	if err := os.MkdirAll(filepath.Join(good, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "bin", "snowflake"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Pack = good
	o = runOpts{}
	if lerr := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); lerr != nil {
		t.Fatalf("a buildable proxy must launch fine: %v", lerr)
	}
	if len(o.PackKits) != 1 {
		t.Fatalf("expected exactly one stacked pack kit, got %v", o.PackKits)
	}
	if _, err := os.Stat(filepath.Join(o.PackKits[0], "files", "usr", "local", "bin", "snowflake")); err != nil {
		t.Errorf("stacked kit is missing the wrapper: %v", err)
	}
}
