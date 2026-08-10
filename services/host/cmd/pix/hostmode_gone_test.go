// hostmode_gone_test.go — the sentinel for U03B (delete host mode + dormant
// broker): a durable, regression-proof assertion that NO code path anywhere in
// the repo can execute `pix host` (the unsandboxed escape hatch) or dispense
// the deleted CredentialBroker plugin capability, so a future change can never
// silently resurrect either without this test noticing.
//
// retired_test.go already proves the CLI SURFACE answers PIX_RETIRED and exit
// 2 (TestRunHostAnswersRetired below re-confirms the actual subprocess
// behavior); this file proves the deeper claim — that the EXECUTION machinery
// those retired surfaces used to reach was actually deleted, not merely
// hidden behind a retirement message. A grep-based sentinel (mirroring
// scripts/check-open-core.sh's pattern) is deliberately blunt: it does not
// need to understand Go to catch the one thing that matters — the named
// symbols/files never coming back.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var (
	pixBinOnce sync.Once
	pixBinPath string
	pixBinErr  error
)

// buildPixBinary compiles the real cmd/pix binary once per test process (a
// LOCAL, unexported twin of corpus's buildPixBinary — corpus deliberately does
// not import cmd/pix, and vice versa, so each test package that needs a real
// compiled subprocess builds its own). Building the actual binary, not
// re-invoking the test binary, is the point: it proves the SHIPPED dispatch
// path, not a test-only shortcut.
func buildPixBinary(t *testing.T) string {
	t.Helper()
	pixBinOnce.Do(func() {
		dir, err := os.MkdirTemp("", "pix-hostmode-sentinel")
		if err != nil {
			pixBinErr = err
			return
		}
		bin := filepath.Join(dir, "pix-host")
		cmd := exec.Command("go", "build", "-o", bin, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			pixBinErr = fmt.Errorf("go build cmd/pix: %v\n%s", err, out)
			return
		}
		pixBinPath = bin
	})
	if pixBinErr != nil {
		t.Fatalf("build pix binary: %v", pixBinErr)
	}
	return pixBinPath
}

// hostModeRoot resolves the services/host module root (three levels up from
// cmd/pix) so the sentinel can grep the WHOLE host module, not just this
// package.
func hostModeRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("resolved %s does not look like the services/host module root: %v", root, err)
	}
	return root
}

// forbiddenHostModeSymbols are identifiers that only ever existed to launch or
// provision `pix host`. Their presence anywhere in a non-test .go file under
// services/host means the escape hatch grew back.
var forbiddenHostModeSymbols = []string{
	"func RunHost(",
	"func ParseHostArgs(",
	"func runHostLaunch(",
	"func runHostSetup(",
	"func ProvisionHostAgentDir(",
	"func HostChildEnv(",
	"func BuildHostArgs(",
	"CredentialBroker interface",
	"type BrokerPlugin struct",
}

// forbiddenKnowledgeSymbols are identifiers that only ever existed to serve
// the built-in OKF knowledge service (W2/U03A), which bound :11436 the same
// way memory binds :11435. Deleted alongside host mode/broker in spirit (both
// are "no supervised unit, no config key, no code path dispenses this any
// more" retirements — see AGENTS.md's go-plugin+Suture section) but recorded
// as its own list: a knowledge symbol reappearing is a DIFFERENT regression
// than a host-mode one, and a single failure message that named the wrong
// retirement would send whoever sees it hunting in the wrong package.
// KNOWLEDGE_PORT (bare, so it also catches PIX_KNOWLEDGE_PORT) is the one
// literal here that is not a Go identifier: it is the env var name every
// resurrection of the port would have to reintroduce somewhere — servicePort,
// env(), or a supervised-unit spec — so it stands in for "the :11436 default"
// without hardcoding the bare port number itself, which still appears
// (correctly) in retrospective doc comments like config.go's knowledge_bundles
// note and would make a bare "11436" check fire on those instead of on code.
var forbiddenKnowledgeSymbols = []string{
	"func RunKnowledge(",
	"func runKnowledgeServe(",
	"func newKnowledgeMux(",
	"type KnowledgeStore",
	"KnowledgePortDefault",
	"KnowledgeClient(",
	"KnowledgeBundles",
	"KNOWLEDGE_PORT",
}

// forbiddenSymbolViolations walks root for non-test .go files containing any
// of symbols, returning one "relpath: symbol" string per hit. Pure — no
// *testing.T — precisely so TestForbiddenSymbolSentinelDetectsAPlantedViolation
// below can assert it actually returns something on a planted violation,
// instead of the sentinel only ever having been observed passing (the same
// "a guard nobody has seen work" concern arch_test.go's sibling-workflow test
// documents for the layering checker).
func forbiddenSymbolViolations(root string, symbols []string) ([]string, error) {
	var violations []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "testdata" || info.Name() == "corpus" {
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
		content := string(b)
		for _, sym := range symbols {
			if strings.Contains(content, sym) {
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, fmt.Sprintf("%s: %s", rel, sym))
			}
		}
		return nil
	})
	return violations, err
}

// TestNoHostModeExecutionSymbols greps every non-test .go file under
// services/host for the deleted host-mode/broker symbols. This is the
// sentinel: it fails loudly the moment any of them is reintroduced, wherever
// in the tree that happens.
func TestNoHostModeExecutionSymbols(t *testing.T) {
	root := hostModeRoot(t)
	violations, err := forbiddenSymbolViolations(root, forbiddenHostModeSymbols)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, v := range violations {
		t.Errorf("%s — `pix host` and the dormant CredentialBroker plugin were deleted; this must not come back", v)
	}
}

// TestNoKnowledgeExecutionSymbols is TestNoHostModeExecutionSymbols' sibling
// for W2/U03A: the built-in :11436 OKF knowledge service (its RPC client, its
// port default, its config bundle list, its serve mux) was deleted outright,
// same standard as host mode/broker — no config key, no supervised unit, no
// code path dispenses it (see AGENTS.md's go-plugin+Suture section, which
// names this exact sentinel file for host mode and calls out knowledge as
// deleted the same way).
func TestNoKnowledgeExecutionSymbols(t *testing.T) {
	root := hostModeRoot(t)
	violations, err := forbiddenSymbolViolations(root, forbiddenKnowledgeSymbols)
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, v := range violations {
		t.Errorf("%s — the built-in :11436 knowledge service (W2/U03A) was deleted; this must not come back", v)
	}
}

// TestForbiddenSymbolSentinelDetectsAPlantedViolation is the plausibility
// assertion for BOTH sentinels above: it plants one forbidden symbol from
// each list into a throwaway tree and proves forbiddenSymbolViolations
// actually reports it, then proves the SAME symbol in a _test.go file is
// correctly ignored (mirroring the real walk's test-file exclusion) — so a
// passing TestNoHostModeExecutionSymbols/TestNoKnowledgeExecutionSymbols is
// evidence the code is clean, not evidence the check forgot how to fail.
func TestForbiddenSymbolSentinelDetectsAPlantedViolation(t *testing.T) {
	dir := t.TempDir()
	planted := filepath.Join(dir, "planted.go")
	if err := os.WriteFile(planted, []byte("package x\n\nfunc RunHost() {}\n\nfunc RunKnowledge() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hostViolations, err := forbiddenSymbolViolations(dir, forbiddenHostModeSymbols)
	if err != nil {
		t.Fatal(err)
	}
	if len(hostViolations) == 0 {
		t.Fatal("expected forbiddenSymbolViolations to catch the planted func RunHost(), but it found nothing — a sentinel that never fires has never been proven to work")
	}

	knowledgeViolations, err := forbiddenSymbolViolations(dir, forbiddenKnowledgeSymbols)
	if err != nil {
		t.Fatal(err)
	}
	if len(knowledgeViolations) == 0 {
		t.Fatal("expected forbiddenSymbolViolations to catch the planted func RunKnowledge(), but it found nothing")
	}

	// The exact same symbol in a _test.go file must NOT be reported: a test
	// legitimately naming a retired symbol (as this very file's forbidden lists
	// do) must never trip the sentinel on itself.
	testFile := filepath.Join(dir, "planted_test.go")
	if err := os.WriteFile(testFile, []byte("package x\n\nfunc RunHost() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(planted) // isolate: only the _test.go file remains
	isolated, err := forbiddenSymbolViolations(dir, forbiddenHostModeSymbols)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated) != 0 {
		t.Fatalf("forbiddenSymbolViolations must ignore _test.go files, got: %v", isolated)
	}
}

// TestNoExtensionBranchesOnHostMode is the half the suite above missed. It
// proved host-guard.ts was deleted, but not that the ENV VAR it armed on stopped
// being read: status.ts still pinned a permanent "HOST -- no sandbox, real
// machine, real credentials" badge, and ollama-bridge.ts still had a branch that
// skipped starting its reverse proxy, both keyed on OLLAMA_HOSTMODE=1. Nothing
// in production Go has set that variable since `pix host` was deleted, so both
// were permanently-false branches describing a mode that cannot exist -- exactly
// the residue a deletion leaves on the OTHER side of a process boundary, where
// `deadcode` (Go-only) cannot see it.
func TestNoExtensionBranchesOnHostMode(t *testing.T) {
	root := hostModeRoot(t)
	extDir := filepath.Join(root, "..", "..", "extensions")
	entries, err := os.ReadDir(extDir)
	if err != nil {
		t.Fatalf("read extensions dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		seen++
		b, rerr := os.ReadFile(filepath.Join(extDir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		if strings.Contains(string(b), "OLLAMA_HOSTMODE") {
			t.Errorf("extensions/%s still branches on OLLAMA_HOSTMODE; `pix host` is deleted and nothing sets it", e.Name())
		}
	}
	if seen == 0 {
		t.Fatal("scanned no extensions; the path has drifted and this guard proves nothing")
	}
}

// TestHostGuardExtensionDeleted proves the sandbox-side half of the deletion:
// extensions/host-guard.ts (the tool_call guard that ONLY ever armed itself
// under OLLAMA_HOSTMODE=1, and whose mere presence on disk was what convinced
// the Go launcher it was safe to run `pix host` unguarded) is gone from the
// repo entirely — not merely disarmed.
func TestHostGuardExtensionDeleted(t *testing.T) {
	root := hostModeRoot(t)
	// services/host -> repo root is two levels up.
	repoRoot := filepath.Join(root, "..", "..")
	guard := filepath.Join(repoRoot, "extensions", "host-guard.ts")
	if _, err := os.Stat(guard); err == nil {
		t.Fatalf("%s still exists; `pix host` was deleted and its guard extension must go with it", guard)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat %s: %v", guard, err)
	}
}

// TestRunHostRefusesAndExecutesNothing is the end-to-end proof: the compiled
// binary, invoked with the exact argv a user would type, refuses with exit 2
// and — the part no source-level string check can prove — never got far enough
// to touch a config file, spawn pi, or do anything else. It is a real
// subprocess, so it also proves no OTHER dispatch path (an alias, a forgotten
// case) reaches host execution.
//
// It used to additionally require the PIX_RETIRED marker. That assertion went
// with the retirement mechanism: `host` is now simply not a verb, so it gets
// the ordinary unknown-command answer. The SAFETY half is what mattered and it
// is unchanged — refuses, and does nothing on the way out.
func TestRunHostRefusesAndExecutesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real pix binary for a CLI roundtrip; TestNoHostModeExecutionSymbols and TestHostGuardExtensionDeleted keep the sentinel cheap in the fast gate; this one is covered by the untimed race/metrics CI jobs")
	}
	bin := buildPixBinary(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	cmd := exec.Command(bin, "host")
	cmd.Env = append(os.Environ(), "PIX_CONFIG="+cfgPath, "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_STATE_HOME="+dir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("`pix host` must exit non-zero; output:\n%s", out)
	}
	if exitErr, ok := err.(*exec.ExitError); !ok || exitErr.ExitCode() != 2 {
		t.Errorf("`pix host` exit code = %v, want 2", err)
	}
	if _, statErr := os.Stat(cfgPath); !os.IsNotExist(statErr) {
		t.Errorf("`pix host` must never write config.toml; stat err = %v", statErr)
	}
}
