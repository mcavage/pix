// deletion_sentinel_test.go is the grep-based, blunt sentinel for the Pix v2
// deletion sweep (docs/design/pix-v2-architecture.md §14, AC-16): it proves
// the deleted subsystems' NAMES never come back into the source tree, the
// same pattern hostmode_gone_test.go and scripts/check-open-core.sh already
// use. It does not need to understand Go — it only needs the named
// symbols/imports/dependencies to never reappear.
//
// What this file does NOT claim: it does not re-prove QA F5 (the old XDG
// config-path-splitting fallback in package config, the ledger's "old config
// XDG" item). That gap is closed separately — config.Path/StateDir/DataDir/
// ContextDir now resolve under PIX_HOME alone, with no PIX_CONFIG/XDG_*
// fallback in production — and pinned by config's own tests plus
// workflow/launch's TestFullLifecycle_EverythingResolvesUnderPIX_HOME_NoXDGEscape
// integration test, not by this sentinel. This sentinel only pins what the
// v2 deletion sweep itself removed.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// hostRoot is services/host, this file's own directory.
func hostRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// walkGoSource visits every non-test, non-vendor .go file under root.
func walkGoSource(t *testing.T, root string, fn func(path, content string)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "corpus" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		fn(path, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDeletionSweep_NoPixHostBinaryOrDispatcher proves the pix-host binary's
// own entry point (package main's `subcommand := os.Args[1]`-style dispatch
// for memory/serve/plugin/uat-mcp/uat-worker) is gone: services/host's root
// directory carries no production .go file at all any more (see arch_test.go's
// "." placement note), so there is no dispatcher left to answer `pix-host
// serve|memory|plugin|uat-mcp|uat-worker`.
func TestDeletionSweep_NoPixHostBinaryOrDispatcher(t *testing.T) {
	root := hostRoot(t)
	files, err := filepath.Glob(filepath.Join(root, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var prod []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			prod = append(prod, f)
		}
	}
	if len(prod) != 0 {
		t.Errorf("services/host root carries production .go files again: %v; the pix-host binary was deleted outright (AC-16) and must not come back", prod)
	}
}

// TestDeletionSweep_NoRemovedImportPaths proves no Go file anywhere in the
// module imports one of the deleted packages: the pack system
// (packinfo, workflow/pack, workflow/uat), the Suture-supervised process
// mechanism (service, supervise, plugin), and the custom memory JSON-RPC
// client (rpc). "pix/host/memory" (the OLD host-side memory CLI + JSON-RPC
// client) is checked as an import path, never as a bare substring, so it
// cannot false-positive on "pix/host/memory" appearing inside a comment
// about the unrelated services/memory module.
func TestDeletionSweep_NoRemovedImportPaths(t *testing.T) {
	bannedImports := []string{
		`"pix/host/rpc"`,
		`"pix/host/packinfo"`,
		`"pix/host/plugin"`,
		`"pix/host/service"`,
		`"pix/host/supervise"`,
		`"pix/host/memory"`,
		`"pix/host/uat"`,
		`"pix/host/uatmatrix"`,
		`"pix/host/uatenvmatrix"`,
		`"pix/host/unitreport"`,
		`"pix/host/unitreport/unitreporttest"`,
		`"pix/host/workflow/pack"`,
		`"pix/host/workflow/uat"`,
	}
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	walkGoSource(t, hostRoot(t), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return // this file legitimately quotes the banned strings above
		}
		for _, imp := range bannedImports {
			if strings.Contains(content, imp) {
				t.Errorf("%s imports the deleted package %s (Pix v2 deletion sweep, AC-16)", path, imp)
			}
		}
	})
}

// TestDeletionSweep_NoServeOrPackVerbInHelpText proves the launcher never
// prints a "pix serve" / "pix pack" / "pix mcp" / "pix models" / "pix agent" /
// "pix status" / "pix uat" invocation as documented, discoverable behavior —
// root.go's own verb table is the only source of truth, and this scans its
// generated help/description strings rather than the verb table itself (a
// verb table check belongs to root_test.go; this is the deletion sweep's
// narrower "the old prose never comes back" claim).
func TestDeletionSweep_NoServeOrPackVerbInHelpText(t *testing.T) {
	removedVerbInvocation := regexp.MustCompile(`\bpix (serve|pack|status|uat)\b`)
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	walkGoSource(t, filepath.Join(hostRoot(t), "cmd", "pix"), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return
		}
		if strings.HasSuffix(path, "_test.go") {
			return // tests may legitimately assert these verbs are ABSENT
		}
		if m := removedVerbInvocation.FindString(content); m != "" {
			t.Errorf("%s documents %q as a runnable invocation; that verb is not in the v2 surface (docs/design/pix-v2-surface.md)", path, m)
		}
	})
}

// TestDeletionSweep_ModuleHasNoSutureOrGoPlugin proves the Go module
// dependency graph itself carries neither github.com/hashicorp/go-plugin nor
// github.com/thejerf/suture/v4 (AC-16, architecture.md §14): `go mod tidy`
// removed both from go.mod/go.sum when their only importers (plugin/,
// supervise/, serve_plugin.go) were deleted, and this pins that they do not
// quietly return as a transitive or re-added direct dependency.
func TestDeletionSweep_ModuleHasNoSutureOrGoPlugin(t *testing.T) {
	for _, rel := range []string{"go.mod", "go.sum"} {
		b, err := os.ReadFile(filepath.Join(hostRoot(t), rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, banned := range []string{"github.com/hashicorp/go-plugin", "github.com/thejerf/suture"} {
			if strings.Contains(string(b), banned) {
				t.Errorf("%s still names %q; it must be absent from the dependency graph (AC-16)", rel, banned)
			}
		}
	}
}
