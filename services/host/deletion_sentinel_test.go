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

// TestDeletionSweep_RemovedVerbsRouteNowhere is the round-4 strengthening of
// this sentinel: it pins the FULL removed verb set, not just the packs/host
// subset the original sweep covered. A removed verb gets the ordinary
// unknown-command answer (AGENTS.md's command surface: pix has no released
// users to keep a migration path for), so the proof is that no dispatch
// table anywhere in the module names one.
func TestDeletionSweep_RemovedVerbsRouteNowhere(t *testing.T) {
	// Each entry is the STRUCT-FIELD shape a kong-style dispatch child takes
	// in cmd/pix/root.go, so this cannot false-positive on the word "config"
	// or "models" appearing in prose.
	removed := []string{
		"mcpCmd struct",
		"modelsCmd struct",
		"configCmd struct",
		"agentCmd struct",
		"packCmd struct",
		"serveCmd struct",
		"resumeCmd struct",
		"statusCmd struct",
		"uatCmd struct",
		"memoryCmd struct",
	}
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	walkGoSource(t, hostRoot(t), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return
		}
		for _, verb := range removed {
			if strings.Contains(content, verb) {
				t.Errorf("%s declares %q: that verb was deleted in the Pix v2 cutover and must not route anywhere", path, verb)
			}
		}
	})
}

// TestDeletionSweep_NoSecondConfigSchemaOrMCPChannel pins round 5's config
// collapse: config.Config is the SOLE config.toml owner (the round-4
// pixhome.Machine duplicate that competed with it for the same file's bytes
// is deleted, not merely unused), config.Config carries no services/mcp/pack
// list, and nothing writes config.toml through a second schema or a legacy
// Save path. A second schema over one file is how two writers disagree
// about the same bytes — exactly the bug pixhome.Machine.SaveMachine had:
// it silently dropped VersionPin/Inference every time `pix env default` ran.
func TestDeletionSweep_NoSecondConfigSchemaOrMCPChannel(t *testing.T) {
	banned := map[string]string{
		"MachineConfig":       "the duplicate config.toml schema (config.Config is the sole owner)",
		"pixhome.Machine":     "the duplicate config.toml schema deleted in round 5 (config.Config is the sole owner)",
		"LoadMachine(":        "the deleted pixhome-based config.toml reader (config.Load/config.LoadFrom is the sole reader)",
		"SaveMachine(":        "the deleted pixhome-based config.toml writer (config.Save is the sole writer)",
		"cfg.MCP":             "config.toml's retired MCP server list (the environment document is the ONE declaration channel)",
		"RegisterServers(":    "the pack-manifest MCP registration admin surface",
		"ReconcileOnboarding": "the in-sandbox onboarding proposal's host-config write path",
	}
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	walkGoSource(t, hostRoot(t), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return
		}
		if strings.HasSuffix(path, "_test.go") {
			return // a test may legitimately name what it proves absent
		}
		for sym, why := range banned {
			if strings.Contains(content, sym) {
				t.Errorf("%s mentions %q: %s was deleted in round 4 and must not come back", path, sym, why)
			}
		}
	})
}

// TestDeletionSweep_NoStatusVerbResidue is the PM P2 AC-16 closeout: `pix
// status` (and the bare-`pix` landing screen it once also served) is not
// part of the v2 CLI surface, and the unreachable library it left behind
// (workflow/doctor's RenderStatus/StatusDescription/RenderStatusConfigError,
// health.RenderStatus underneath them, and their status-only tests) is
// deleted, not merely hidden. Three patterns are banned:
//
//   - `RenderStatusConfigError(` and `StatusDescription` as declarations/uses
//     (not bare prose mentions — a comment explaining the deletion may still
//     name the OLD identifier without a trailing call/reference form).
//   - `ConfigLoadSnapshot` bare: this was status.go's name for the one piece
//     that survives the sweep (a required config-load-failure Snapshot that
//     `pix doctor` itself renders); it now lives in doctor.go as
//     UnreadableConfigSnapshot, and the OLD name must not come back.
//   - the exact Go string literal `"pix config path"`: that verb does not
//     exist in v2 (there is no `config` command at all), so nothing may hand
//     it to a user as a Fix/command value. Backtick prose mentions inside a
//     comment explaining WHY it does not exist are a different, longer
//     substring and do not match this exact quoted literal.
func TestDeletionSweep_NoStatusVerbResidue(t *testing.T) {
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	bannedExact := []string{"ConfigLoadSnapshot", "StatusDescription"}
	bannedCallOrLiteral := regexp.MustCompile(`RenderStatusConfigError\(|"pix config path"`)
	walkGoSource(t, hostRoot(t), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return
		}
		for _, sym := range bannedExact {
			if strings.Contains(content, sym) {
				t.Errorf("%s mentions %q: the removed `pix status` verb's library was deleted outright (AC-16) and must not come back", path, sym)
			}
		}
		if m := bannedCallOrLiteral.FindString(content); m != "" {
			t.Errorf("%s uses %s: the removed `pix status` verb's library was deleted outright (AC-16) and must not come back", path, m)
		}
	})
}

// TestDeletionSweep_OpRefsResolvesUnderPixHomeOnly is QA F5's closing
// sentinel: no production file may consult $PIX_CONFIG or any XDG_* variable
// to locate a Pix-owned file. PIX_HOME is the single root (safety invariant 1).
func TestDeletionSweep_OpRefsResolvesUnderPixHomeOnly(t *testing.T) {
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	bad := regexp.MustCompile(`Getenv\("(PIX_CONFIG|XDG_[A-Z_]+)"\)`)
	walkGoSource(t, hostRoot(t), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return
		}
		if m := bad.FindString(content); m != "" {
			t.Errorf("%s reads %s: Pix resolves every one of its own files under PIX_HOME alone", path, m)
		}
	})
}

// TestDeletionSweep_NoGlobalSbxSecretWrite is Wave C's sentinel: Pix never
// writes a GLOBAL sbx secret. A global (`sbx secret set -g <name>`) is
// host-wide — every sandbox on the machine reads it, including ones another
// PIX_HOME created — so it makes a host-wide store the real owner of a
// PIX_HOME's credentials, hides rotations behind a recreate, and lets a second
// stack inherit the first one's keys. The scoped form (`-f --sandbox <name>`)
// is the only write shape, and secret.setScopedSbxSecret is the only site.
//
// Two claims, both grep-shaped for the same reason the rest of this file is:
// the NAMES must not come back, whatever the code around them looks like.
func TestDeletionSweep_NoGlobalSbxSecretWrite(t *testing.T) {
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	// A `sbx secret set` argv with a global flag, in any of the spellings Go
	// source can produce it in.
	globalWrite := regexp.MustCompile(`"secret",\s*"set"[^)]*"(-g|--global)"|secret set -f -g|secret set -g`)
	var writeSites []string
	walkGoSource(t, hostRoot(t), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return
		}
		if strings.HasSuffix(path, "_test.go") {
			return // a test may legitimately assert the global form is ABSENT
		}
		if m := globalWrite.FindString(content); m != "" {
			t.Errorf("%s writes a GLOBAL sbx secret (%s): Pix writes sandbox-scoped secrets only", path, m)
		}
		if strings.Contains(content, `"secret", "set"`) {
			writeSites = append(writeSites, path)
		}
	})
	// ONE write site, so the flags and the leak posture cannot diverge.
	if len(writeSites) != 1 || !strings.HasSuffix(writeSites[0], filepath.Join("secret", "scoped.go")) {
		t.Errorf("`sbx secret set` is invoked from %v; the only permitted site is secret/scoped.go's setScopedSbxSecret", writeSites)
	}
}

// TestDeletionSweep_NoGlobalSecretSyncSurface proves the global-sync internals
// are deleted, not merely unreferenced: there is no function that pushes this
// host's refs into the host-wide store, and no `pix secret sync` verb came
// back to call one.
func TestDeletionSweep_NoGlobalSecretSyncSurface(t *testing.T) {
	self, err := filepath.Abs("deletion_sentinel_test.go")
	if err != nil {
		t.Fatal(err)
	}
	banned := map[string]string{
		"syncProviderKeys":           "the global provider-key sync",
		"EnsureProviderKeysFromRefs": "the lazy global fill",
		"setSbxSecret(":              "the global secret writer",
		"syncCmd struct":             "the removed `pix secret sync` verb",
	}
	walkGoSource(t, hostRoot(t), func(path, content string) {
		if abs, _ := filepath.Abs(path); abs == self {
			return
		}
		if strings.HasSuffix(path, "_test.go") {
			return // a test may legitimately name what it proves absent
		}
		for sym, why := range banned {
			if strings.Contains(content, sym) {
				t.Errorf("%s mentions %q: %s was deleted in the credential de-globalization and must not come back", path, sym, why)
			}
		}
	})
}
