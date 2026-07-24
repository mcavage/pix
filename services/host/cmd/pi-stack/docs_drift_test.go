package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot locates the repository root from THIS test file's own source path
// (runtime.Caller), so it works regardless of `go test`'s working directory.
// It walks upward looking for the marker files every checkout has (README.md
// alongside a pi-kit/ directory), rather than hard-coding a directory depth
// that would silently break if this package ever moved.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 10; i++ {
		if info, err := os.Stat(filepath.Join(dir, "README.md")); err == nil && !info.IsDir() {
			if fi, err := os.Stat(filepath.Join(dir, "pi-kit")); err == nil && fi.IsDir() {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate repo root (looked for README.md + pi-kit/ walking up from this test file)")
	return ""
}

// readRepoFile reads a repo-root-relative file as a string, failing the test
// (not erroring silently) if it's missing — a moved/renamed doc should break
// this guard loudly rather than have it quietly stop checking anything.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

// assertNone fails the test if any of terms appears (case-sensitive, plain
// substring) in page, labeling the failure with label and the doc name.
func assertNone(t *testing.T, doc, page string, terms ...string) {
	t.Helper()
	for _, term := range terms {
		if strings.Contains(page, term) {
			t.Errorf("%s still contains stale term %q (see U4 doc-sync task)", doc, term)
		}
	}
}

// assertAll fails the test if any of terms is MISSING from page — the
// positive half of the same guard: a current flag/command/default that must
// be documented somewhere in the doc.
func assertAll(t *testing.T, doc, page string, terms ...string) {
	t.Helper()
	for _, term := range terms {
		if !strings.Contains(page, term) {
			t.Errorf("%s is missing %q (should document current behavior)", doc, term)
		}
	}
}

// TestManPageNoStaleTerms is the anti-drift guardrail for the embedded man
// page (U4): it must not resurrect the removed --use-sbx-keys flag, the
// removed profiles/active_profile config, the false %20-encoding claim for
// op-refs, or the stale gemma3:4b watcher-model default, and it MUST document
// the current doctor/setup/gog flags and default.
func TestManPageNoStaleTerms(t *testing.T) {
	page := string(manPage)
	// active_profile/profiles.* are deliberately NOT in this list: the page
	// legitimately names them once, in the FILES section, to say plainly
	// that neither exists anymore (profiles were removed). That is the
	// correct current behavior, not drift.
	assertNone(t, "pi-stack.1", page,
		`\-\-use\-sbx\-keys`,
		"use-sbx-keys",
		"gemma3:4b",
	)
	if strings.Contains(page, ".BR active_profile") {
		t.Error("pi-stack.1 documents active_profile as a live schema field (.BR active_profile) — it was removed")
	}
	if strings.Contains(page, ".BR profiles.*") {
		t.Error("pi-stack.1 documents profiles.* as a live schema field (.BR profiles.*) — profiles were removed")
	}
	assertAll(t, "pi-stack.1", page,
		"qwen3.5:9b",
		`\-\-verbose`,
		`\-\-pull\-models`,
		"pi-stack gog setup",
	)
}

// TestReadmeNoStaleTerms guards README.md's first-run and local-model
// sections: the old `gog auth login` recipe and `make mcp-register` as the
// gog consumer path must be gone, replaced by `pi-stack gog setup`; the
// current doctor/setup behavior must be documented.
func TestReadmeNoStaleTerms(t *testing.T) {
	page := readRepoFile(t, "README.md")
	assertNone(t, "README.md", page,
		"gog auth login",
		"make mcp-register",
		"--use-sbx-keys",
	)
	assertAll(t, "README.md", page,
		"pi-stack gog setup",
		"--pull-models",
		"--verbose",
		"qwen3.5:9b",
	)
}

// TestGogSetupGuideNoStaleTerms guards docs/gog-setup.md (U4): the rewritten
// guide must drop the SBX_MCP_URL/unreleased-gateway framing, the
// repo-relative config/op-refs.env path, gws teardown instructions, and
// `make mcp-register` as the consumer path, and must document the guided
// `pi-stack gog setup` command plus the headless keyring trap.
func TestGogSetupGuideNoStaleTerms(t *testing.T) {
	page := readRepoFile(t, "docs/gog-setup.md")
	assertNone(t, "docs/gog-setup.md", page,
		"SBX_MCP_URL",
		"not yet publicly released",
		"config/op-refs.env",
		"make mcp-register",
		"gws auth logout",
	)
	assertAll(t, "docs/gog-setup.md", page,
		"pi-stack gog setup",
		"--account",
		"--credentials",
		"pi-stack mcp load gog",
		"pi-stack doctor",
	)
}

// TestOnboardingSkillNoStaleTerms guards skills/onboarding/SKILL.md (U4): the
// incomplete gog config-set recipe and the redundant host.enabled follow-up
// after `pi-stack host setup` must be gone.
func TestOnboardingSkillNoStaleTerms(t *testing.T) {
	page := readRepoFile(t, "skills/onboarding/SKILL.md")
	assertNone(t, "skills/onboarding/SKILL.md", page,
		"config set mcp gog",
		"config set gog_account",
	)
	assertAll(t, "skills/onboarding/SKILL.md", page,
		"pi-stack gog setup",
		"pi-stack doctor",
	)
}

// TestHistoricalDesignDocsAreBannered (U4) makes sure the two historical
// onboarding design docs carry a prominent superseded banner pointing at the
// current implementation, so a reader lands on the live behavior instead of
// mistaking a design doc for it. It checks for the banner only — it never
// asserts anything about the historical content beneath it.
func TestHistoricalDesignDocsAreBannered(t *testing.T) {
	for _, rel := range []string{
		"docs/design/onboarding.md",
		"docs/design/onboarding-v2-spec.md",
	} {
		page := readRepoFile(t, rel)
		if !strings.Contains(page, "Historical, superseded") {
			t.Errorf("%s is missing a prominent historical/superseded banner", rel)
		}
		if !strings.Contains(page, "skills/onboarding/SKILL.md") {
			t.Errorf("%s's banner should link to the current implementation (skills/onboarding/SKILL.md)", rel)
		}
	}
}

// TestCliRedesignDocIsBannered (R1-13) guards docs/design/cli-redesign.md:
// the doc labels itself IMPLEMENTED, but its body still contains removed
// `--profile`/`profile` examples and the obsolete direct `gog auth login`
// flow. The narrow, safe fix is a prominent top banner marking those parts
// historical/superseded and pointing at the current docs, not a rewrite of
// the historical body — so this test checks ONLY for the banner and its
// links to the current docs (README.md, `pi-stack help`, docs/gog-setup.md),
// and deliberately never scans the historical body below the banner for
// `--profile`/`gog auth login` (those terms are EXPECTED there, as labeled
// historical illustrations of the shape being proposed).
func TestCliRedesignDocIsBannered(t *testing.T) {
	const rel = "docs/design/cli-redesign.md"
	page := readRepoFile(t, rel)
	if !strings.Contains(page, "Historical, superseded") {
		t.Errorf("%s is missing a prominent historical/superseded banner", rel)
	}
	assertAll(t, rel, page,
		"README.md",
		"pi-stack help",
		"docs/gog-setup.md",
	)
}
