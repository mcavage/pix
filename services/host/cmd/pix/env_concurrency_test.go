package main

// env_concurrency_test.go — Wave C security L1 at the dispatch layer: a
// *cli.Deps whose cached config went stale (another pix process committed a
// registration in between) must never clobber that commit when `env use` /
// `env forget` persist theirs. The workflow-layer interleavings live in
// workflow/env/commit_test.go; this file proves the END-TO-END path (the
// SAME kong dispatch production uses, including Deps' config cache) commits
// through the shared env-registry lock's fresh reload.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
)

// otherPixProcessRegisters models a concurrent process's completed commit
// against the live $PIX_CONFIG: fresh load, one registration, save.
func otherPixProcessRegisters(t *testing.T, name string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".sbxenv.yaml"), []byte("schemaVersion: \"1\"\nagent: pix\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.AddEnvironment(name, root); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestEnvUse_StaleDepsCacheCannotLoseConcurrentRegistration(t *testing.T) {
	d, _, errb := envDeps(t)
	registerTier0Env(t, "home")
	if _, err := d.Config(); err != nil { // warm the Deps cache; it goes stale next
		t.Fatal(err)
	}

	otherPixProcessRegisters(t, "late")

	if code := dispatch([]string{"env", "use", "home"}, d); code != 0 {
		t.Fatalf("pix env use home = %d, want 0 (stderr: %s)", code, errb.String())
	}

	fresh, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Environment != "home" {
		t.Errorf("environment = %q, want %q", fresh.Environment, "home")
	}
	if _, ok := fresh.Environments["late"]; !ok {
		t.Error("the concurrent process's registration was lost (lost update through the stale Deps cache)")
	}
}

func TestEnvForget_StaleDepsCacheCannotLoseConcurrentRegistration(t *testing.T) {
	d, _, errb := envDeps(t)
	registerTier0Env(t, "home")
	registerTier0Env(t, "doomed")
	if _, err := d.Config(); err != nil {
		t.Fatal(err)
	}

	otherPixProcessRegisters(t, "late")

	if code := dispatch([]string{"env", "forget", "doomed"}, d); code != 0 {
		t.Fatalf("pix env forget doomed = %d, want 0 (stderr: %s)", code, errb.String())
	}

	fresh, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fresh.Environments["doomed"]; ok {
		t.Error("doomed must be unregistered")
	}
	for _, want := range []string{"home", "late"} {
		if _, ok := fresh.Environments[want]; !ok {
			t.Errorf("%q was lost (lost update through the stale Deps cache)", want)
		}
	}
}

// TestEnvCmd_NoDirectConfigSaveInEnvVerbs is the dispatch-layer sentinel of
// workflow/env's TestEnvRegistryWriters_SaveOnlyInsideLockedCommit: env
// verbs never call cfg.Save() themselves; the locked commit inside
// workflow/env is the ONE place a registry/default mutation persists.
func TestEnvCmd_NoDirectConfigSaveInEnvVerbs(t *testing.T) {
	fset := token.NewFileSet()
	node, err := parser.ParseFile(fset, "env_cmd.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(node, func(x ast.Node) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Save" {
			t.Errorf("env_cmd.go:%d calls .Save() directly; env mutations must commit through workflow/env's locked helper",
				fset.Position(call.Pos()).Line)
		}
		return true
	})
}
