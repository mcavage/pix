// Moved from pack/pack_v2_round4_test.go: the subject is applying a pack to a LAUNCH
// (launch.RunOpts, launch.ApplyPackToLaunch, launch.WritePackContextFiles), which lives in
// launchpack.go on this side of the boundary.
package main

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/workflow/launch"
	"pix/host/workflow/pack"
)

// TestPackAddMcp_LockWriteFailureAbortsWithoutCommit: same guarantee on the
// active-pack `pack add mcp` path — the pre-existing config (active pack, no
// MCP) is left byte-for-byte alone when the lock can't be written.
func TestPackAddMcp_LockWriteFailureAbortsWithoutCommit(t *testing.T) {
	if os.Getenv("PIX_TEST_LOCKFAIL") == "add" {
		pack.RunPackAdd(fakeGitEnv(nil), os.Stdout, []string{"mcp", "fastmail", os.Getenv("PIX_TEST_PACK_ROOT"), "--yes"}, registerServers)
		return
	}
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	root := filepath.Join(dir, "pack")
	mustWritePack(t, root, pack.Manifest{Name: "work", Schema: 1})
	brokenPackLock(t, root)
	// The pack is ACTIVE (that is what routes `pack add mcp` into the attach+
	// commit path).
	before := "pack = \"" + root + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run", "^TestPackAddMcp_LockWriteFailureAbortsWithoutCommit$")
	cmd.Env = append(os.Environ(),
		"PIX_TEST_LOCKFAIL=add",
		"PIX_TEST_PACK_ROOT="+root,
		"PIX_CONFIG="+cfgPath,
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
// a sandbox proxy whose wrapper can't be read makes launch.ApplyPackToLaunch return an
// error (the launch path aborts), never a kitless create — while "no proxies
// declared" and "buildable proxy" both proceed.

// TestApplyPackToLaunch_FailsClosedOnBrokenDeclaredProxy: a pack that DECLARES
// a sandbox proxy whose wrapper can't be read makes launch.ApplyPackToLaunch return an
// error (the launch path aborts), never a kitless create — while "no proxies
// declared" and "buildable proxy" both proceed.
func TestApplyPackToLaunch_FailsClosedOnBrokenDeclaredProxy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}

	// 1) Declared proxy, no bin/<name> on disk: the launch must refuse.
	broken := filepath.Join(dir, "broken-pack")
	mustWritePack(t, broken, pack.Manifest{Name: "broken", Schema: 1, Proxies: []pack.PackProxy{{Name: "warehouse"}}})
	cfg.Pack = broken
	o := launch.RunOpts{}
	if _, lerr := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard); lerr == nil {
		t.Fatal("F2: expected an error for a declared sandbox proxy whose kit can't be built")
	} else if !strings.Contains(lerr.Error(), "refusing") {
		t.Errorf("expected a refusal message, got: %v", lerr)
	}
	if len(o.PackKits) != 0 {
		t.Errorf("no kit may be stacked on a failed synth, got %v", o.PackKits)
	}

	// 2) No proxies declared: fine, no kit, no error.
	plain := filepath.Join(dir, "plain-pack")
	mustWritePack(t, plain, pack.Manifest{Name: "plain", Schema: 1})
	cfg.Pack = plain
	o = launch.RunOpts{}
	if _, lerr := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard); lerr != nil {
		t.Fatalf("a pack with no proxies must launch fine: %v", lerr)
	}
	if len(o.PackKits) != 0 {
		t.Errorf("no kit expected for a proxy-less pack, got %v", o.PackKits)
	}

	// 3) Buildable proxy: kit stacked, no error.
	good := filepath.Join(dir, "good-pack")
	mustWritePack(t, good, pack.Manifest{Name: "good", Schema: 1, Proxies: []pack.PackProxy{{Name: "warehouse"}}})
	if err := os.MkdirAll(filepath.Join(good, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "bin", "warehouse"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg.Pack = good
	o = launch.RunOpts{}
	if _, lerr := launch.ApplyPackToLaunch(cfg, &o, fakeGitEnv(nil), io.Discard); lerr != nil {
		t.Fatalf("a buildable proxy must launch fine: %v", lerr)
	}
	if len(o.PackKits) != 1 {
		t.Fatalf("expected exactly one stacked pack kit, got %v", o.PackKits)
	}
	if _, err := os.Stat(filepath.Join(o.PackKits[0], "files", "home", ".local", "bin", "warehouse")); err != nil {
		t.Errorf("stacked kit is missing the wrapper: %v", err)
	}
}
