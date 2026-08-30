package main

// routing_gone_test.go — the F11 sentinel for Wave F (delete the model
// router): a durable, grep-based assertion that the scored router is GONE from
// the live tree, not merely unreachable from the CLI. Modeled on
// cmd/pix/hostmode_gone_test.go, and blunt on purpose — it does not need to
// understand Go to catch the one thing that matters: the named symbols, files
// and make targets never coming back.
//
// It scans the LIVE tree: production Go, shipped config, extensions, skills,
// docs, Makefile, Dockerfile. It deliberately skips .git (history is not the
// live tree), .pi-agent (delivery evidence records what WAS deleted, quoting
// the very words it deleted), CHANGELOG.md (same reason: a changelog that
// cannot name what it removed is useless), and docs/design/environments.md
// (the Wave F design contract itself, which specifies this deletion).
//
// Test files are scanned too, minus a narrow allowlist for the sentinels that
// must quote the forbidden words to assert their absence.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenRoutingSymbols are the five F11 symbols plus the artifacts and make
// target they came with. Any occurrence in the live tree means the router grew
// back — or that a doc is still telling a user to drive one that is gone.
var forbiddenRoutingSymbols = []string{
	"scorecard",
	"prefer_providers",
	"max_cost_usd",
	"run_intent",
	"CompiledRouting",
	"make routing",
	"routing.json",
	"pix-host route",
	"pix models pick",
	"models show",
	"skills/model-refresh",
	"pix/host/routing",
}

// routingSentinelSkipDirs are the trees this sentinel does not speak for.
var routingSentinelSkipDirs = map[string]bool{
	".git": true, ".pi-agent": true, "node_modules": true, "out": true,
	"dist": true, ".worktrees": true,
}

// routingSentinelSkipFiles are the individual live files allowed to name the
// deleted router, each for a stated reason.
var routingSentinelSkipFiles = map[string]string{
	// The sentinel itself, and the two shape/parser sentinels that must name
	// what they forbid.
	"services/host/routing_gone_test.go":              "this file",
	"services/host/inference/catalog_shape_test.go":   "names the scored fields it bans",
	"services/host/inference/hardware_shape_test.go":  "names the scored fields it bans",
	"services/host/cmd/pix/agent_intent_gone_test.go": "asserts routing.json and the package are absent",
	"services/host/inference/manifest_layout_test.go": "asserts the mixin carries no routing.json",
	"services/host/cmd/pix/config_cmd_test.go":        "asserts `config set run_intent` is refused",
	"services/host/cmd/pix/config_get_test.go":        "asserts `config get run_intent` is refused",
	"services/host/cmd/pix/models_cmd_test.go":        "asserts the scored verbs are unknown commands",
	"tests/no-extension-routing-json-read.test.mjs":   "asserts no extension reads routing.json",
	"tests/subagents-disabled.test.mjs":               "asserts subagents.ts parses no intent frontmatter",
	"CHANGELOG.md":                                    "history names what was removed",
	"docs/design/environments.md":                     "the design contract that specifies this deletion",
	"docs/design/packs-v2-impl.md":                    "historical note: the pack [routing] facet never shipped",
	"docs/design/onboarding-v3.md":                    "historical note about the deleted route compiler",
	"services/host/workflow/models/inference.go":      "comment: RAM facts are the rung table's, not a catalog row's",
	"services/host/cmd/pix/verbcoverage_test.go":      "asserts `models` lists only `add`",
	"services/host/cmd/pix/pack_inference_test.go":    "quotes the historical `pix models ls` bug it fixed",
	"services/host/cmd/pix/redrive_dxfix_test.go":     "names the scorecard half of the finding that is gone",
	"services/host/cmd/pix/models_cmd.go":             "states which scored verbs were deleted and why",
	"services/host/inference/catalog.go":              "states which scored fields the catalog may not carry",
	"services/host/inference/hardware.go":             "states the rung table carries no scored field",
	"AGENTS.md":                                       "states that the router was deleted",
	"docs/reference.md":                               "states that nothing scores a model",
	"services/host/cmd/pix/agent.go":                  "states this command never invokes a scored pick",
	"services/host/cmd/pix/agent_cmd.go":              "states the roster is not scored",
	"services/host/inference/bindings.go":             "states catalog and config are separate",
	"services/host/inference/live.go":                 "states the mixin has no second generated file",
	"extensions/subagents.ts":                         "states there is no compiled router to resolve against",
	"services/host/cmd/pix/hostmode_gone_test.go":     "the sibling sentinel's own symbol table",
	"scripts/arch-metrics/budgets.json":               "budget notes name the packages that were removed",
	"docs/design/ollama-inference.md":                 "historical design doc, banner-marked: it predates the deletion",
	"scripts/semantic-diff/intended-changes.json":     "the semantic pin that DECLARES these removals intended",
	"tests/subagent-roster-resolution.test.mjs":       "asserts the roster comes from inference.json, never routing.json",
	// The v2 deletion ledger. A doc whose JOB is to enumerate what Pix does
	// not have must be allowed to write the names down; a sentinel that
	// forbids naming a deleted concept forbids documenting its deletion,
	// which is how a removal quietly stops being provable.
	"docs/design/pix-v2-surface.md":      "the accepted surface's removed-surfaces ledger names what v2 has no more of",
	"docs/design/pix-v2-architecture.md": "the accepted architecture's deletion ledger names what is deleted, not ported",
}

// repoRootForRoutingSentinel walks up from services/host to the repo root.
func repoRootForRoutingSentinel(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Dir(root)
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("resolved %s does not look like the repo root: %v", root, err)
	}
	return root
}

// scannedRoutingExtensions are the live-tree file kinds this sentinel reads:
// code, config, and prose a user is pointed at.
var scannedRoutingExtensions = map[string]bool{
	".go": true, ".ts": true, ".mjs": true, ".js": true, ".json": true,
	".md": true, ".yaml": true, ".yml": true, ".sh": true, ".toml": true,
}

func scanRoutingSentinel(t *testing.T, check func(rel, content string)) {
	t.Helper()
	root := repoRootForRoutingSentinel(t)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if info.IsDir() {
			if routingSentinelSkipDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		base := filepath.Base(path)
		if !scannedRoutingExtensions[filepath.Ext(path)] && base != "Makefile" && base != "Dockerfile" {
			return nil
		}
		if _, skip := routingSentinelSkipFiles[filepath.ToSlash(rel)]; skip {
			return nil
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		check(filepath.ToSlash(rel), string(b))
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
}

// TestRoutingSymbolsAreGoneFromTheLiveTree is F11 itself.
func TestRoutingSymbolsAreGoneFromTheLiveTree(t *testing.T) {
	scanRoutingSentinel(t, func(rel, content string) {
		for _, sym := range forbiddenRoutingSymbols {
			if strings.Contains(content, sym) {
				t.Errorf("%s still names %q: the model router (scorecard, policy, intents, the compiled map and the CLI that drove it) was DELETED in Wave F, not retired. Remove the reference, or add the file to routingSentinelSkipFiles with a stated reason.", rel, sym)
			}
		}
	})
}

// TestRoutingFilesAndTargetsAreGone pins the artifacts by path, so a revert
// that restores files without re-introducing a symbol still fails.
func TestRoutingFilesAndTargetsAreGone(t *testing.T) {
	root := repoRootForRoutingSentinel(t)
	for _, rel := range []string{
		"routing.json",
		"services/host/routing",
		"services/host/route.go",
		"services/host/route_test.go",
		"skills/model-refresh",
		"docs/design/routing.md",
		"docs/design/models-cli.md",
	} {
		if _, err := os.Stat(filepath.Join(root, rel)); !os.IsNotExist(err) {
			t.Errorf("%s is back (err=%v); Wave F deleted it", rel, err)
		}
	}
}

// TestNoScoredVocabularyInUserVisibleCopy is the softer half: even where a
// symbol is not spelled, a user must never be shown the language of a router
// that no longer exists. Scoped to the shipped prose and copy a user reads.
func TestNoScoredVocabularyInUserVisibleCopy(t *testing.T) {
	banned := []string{"resolved intent", "intent->model", "intent→model", "model router", "--intent"}
	scanRoutingSentinel(t, func(rel, content string) {
		if !strings.HasPrefix(rel, "skills/") && !strings.HasPrefix(rel, "docs/") &&
			rel != "README.md" && !strings.HasPrefix(rel, "extensions/") {
			return
		}
		lower := strings.ToLower(content)
		for _, phrase := range banned {
			if strings.Contains(lower, strings.ToLower(phrase)) {
				t.Errorf("%s still uses the scored-router phrase %q in user-visible copy", rel, phrase)
			}
		}
	})
}
