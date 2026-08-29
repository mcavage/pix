package main

// arch_effective_test.go pins E2.1's two architect corrections with named,
// independently-failing sentinels — TestArchitecture_ImportsPointDown
// (arch_test.go) already enforces envinfo's sibling isolation generically,
// but a reader of THIS unit should see its own specific claim proven, not
// just "the shared L1 rule held two files away".

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestArchitecture_EnvinfoRenderNoSiblingImport is a literal, source-text
// check over envinfo/render.go's own import block: RenderEffective must
// take no dependency on pix/host/mcp, pix/host/sandbox, or any
// pix/host/workflow/* package (AC-54's "do NOT import mcp/sandbox/
// workflow" — this file's own name for it).
func TestArchitecture_EnvinfoRenderNoSiblingImport(t *testing.T) {
	src := mustReadArchFile(t, "envinfo/render.go")
	for _, forbidden := range []string{
		`"pix/host/mcp"`,
		`"pix/host/sandbox"`,
		`"pix/host/workflow/`,
	} {
		if strings.Contains(src, forbidden) {
			t.Errorf("envinfo/render.go must not import %s (E2.1 AC-54: RenderEffective is pure, no sibling/workflow dependency)", forbidden)
		}
	}
}

// TestArchitecture_NoDuplicateOpRunGrammar proves package mcp's OpRunWrap
// (mcp/mcp.go) stays the ONE place this module builds the `op run
// --no-masking --env-file=...` argv (docs/design/environments.md §9.2). A
// second hand-built copy anywhere else — most plausibly in envinfo or
// workflow/env, which this unit adds a new caller to — would let a future
// edit to the grammar silently diverge between what registration spawns
// and what an effective-document preview claims will run. Scoped to
// PRODUCTION (non-`_test.go`) source only: workflow/pack/probewrap_test.go
// and mcp/mcp_test.go already carry pre-existing, legitimate fixture/
// assertion copies of this literal string in test files, and this
// sentinel's whole point is a NEW production duplicate, not those.
func TestArchitecture_NoDuplicateOpRunGrammar(t *testing.T) {
	const grammar = "--no-masking"
	allowed := filepath.FromSlash("mcp/mcp.go")
	var offenders []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if path == allowed {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), grammar) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(offenders) > 0 {
		t.Errorf("op-run grammar %q duplicated outside mcp/mcp.go in production source: %v (reuse mcp.OpRunWrap instead)", grammar, offenders)
	}
}

// TestArchitecture_ExactlyOneEffectiveDocumentProducer pins F17: AC-54
// requires exactly ONE stable effective-document producer,
// envinfo.RenderEffective, that both `pix env show --effective` (today)
// and a future E2.5 launch composition must call — never a second,
// independently-shaped renderer. This greps every production (non-test)
// .go file in the module for a qualified call `envinfo.RenderEffective(`
// and requires exactly one call site, and that it lives in
// workflow/env/effective.go: cmd/pix must reach RenderEffective only
// THROUGH workflow/env's typed wrapper (RenderEffectiveDocument), never by
// calling it directly.
func TestArchitecture_ExactlyOneEffectiveDocumentProducer(t *testing.T) {
	const call = "envinfo.RenderEffective("
	var sites []string
	err := filepath.Walk(".", func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case "node_modules", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), call) {
			sites = append(sites, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(sites) != 1 {
		t.Fatalf("envinfo.RenderEffective must have exactly one production call site (F17); found %v", sites)
	}
	if want := filepath.FromSlash("workflow/env/effective.go"); sites[0] != want {
		t.Fatalf("envinfo.RenderEffective's one call site must be %s (env show and every future launch composition share it); found %s", want, sites[0])
	}

	// cmd/pix must never call envinfo.RenderEffective directly.
	err = filepath.Walk("cmd/pix", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(b), call) {
			t.Errorf("cmd/pix must never call envinfo.RenderEffective directly (must go through workflow/env's typed wrapper): %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cmd/pix: %v", err)
	}
}

func mustReadArchFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
