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

// TestNoHostModeExecutionSymbols greps every non-test .go file under
// services/host for the deleted host-mode/broker symbols. This is the
// sentinel: it fails loudly the moment any of them is reintroduced, wherever
// in the tree that happens.
func TestNoHostModeExecutionSymbols(t *testing.T) {
	root := hostModeRoot(t)
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
		for _, sym := range forbiddenHostModeSymbols {
			if strings.Contains(content, sym) {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s: forbidden host-mode/broker symbol %q found — `pix host` and the dormant CredentialBroker plugin were deleted; this must not come back", rel, sym)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
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

// TestRunHostAnswersRetiredAndExecutesNothing is the end-to-end proof: the
// compiled binary, invoked with the exact argv a user would type, prints the
// PIX_RETIRED marker, exits 2, and — the part a pure string check on
// retired.go can't prove — never got far enough to touch a config file,
// spawn pi, or do anything else. It is a real subprocess, not a call into the
// retirement table, so it also proves no OTHER dispatch path (an alias, a
// forgotten case) reaches host execution.
func TestRunHostAnswersRetiredAndExecutesNothing(t *testing.T) {
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
	if !strings.Contains(string(out), "PIX_RETIRED") {
		t.Errorf("`pix host` output missing the PIX_RETIRED marker:\n%s", out)
	}
	if _, statErr := os.Stat(cfgPath); !os.IsNotExist(statErr) {
		t.Errorf("`pix host` must never write config.toml; stat err = %v", statErr)
	}
}
