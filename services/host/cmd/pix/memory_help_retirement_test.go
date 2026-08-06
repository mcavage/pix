// memory_help_retirement_test.go — the launcher's half of the U07b review
// finding: `services/host/memory/memory.go`'s Usage constant used to claim
// "Backup/restore are now TOP-LEVEL verbs (they cover config + op-refs +
// memory)" and pointed at `pix backup [--out PATH] [--keep N]` / `pix restore
// <archive> [--force]` — flags that never existed post-collapse, verbs that
// are themselves RETIRED, and a false claim that a snapshot backs up
// anything beyond memory.db. These tests run the REAL compiled `pix` binary
// (corpus's shard/retirement suite proves the schema; these prove the actual
// help copy a user reads and the actual retirement chain a user hits) so a
// stale doc string can never pass a unit test that only checks a Go constant
// while the shipped process prints something else.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPixMemoryHelp_DescribesSnapshotRestoreNotFullArchive runs `pix memory
// --help` (and `pix help memory`, a second discovery path through the same
// verbUsage table) against the real binary and asserts the printed copy
// matches the approved PRD: snapshot/restore are pix-host memory primitives
// that back up memory.db only; config.toml is recreated with `pix config`;
// op-refs.env holds references, not secrets. It must NOT resurrect the
// retired top-level `pix backup`/`pix restore` verbs or claim the archive
// covers config/op-refs.
func TestPixMemoryHelp_DescribesSnapshotRestoreNotFullArchive(t *testing.T) {
	bin := buildPixBinary(t)
	dir := t.TempDir()
	env := append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_STATE_HOME="+dir)

	for _, args := range [][]string{{"memory", "--help"}, {"help", "memory"}} {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pix %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		text := string(out)
		for _, want := range []string{"pix-host memory snapshot", "pix-host memory restore", "pix config"} {
			if !strings.Contains(text, want) {
				t.Errorf("pix %s missing %q:\n%s", strings.Join(args, " "), want, text)
			}
		}
		for _, unwanted := range []string{"pix backup", "pix restore", "config + op-refs", "--keep N", "<archive>"} {
			if strings.Contains(text, unwanted) {
				t.Errorf("pix %s resurrects the retired full-archive claim %q:\n%s", strings.Join(args, " "), unwanted, text)
			}
		}
	}
}

// TestPixBackupRestore_RetireToLiveHostReplacement is a real-subprocess
// retirement check: `pix backup`/`pix restore` must exit 2, print a
// PIX_RETIRED marker naming the LIVE replacement DIRECTLY (DX finding 8 --
// the ledger's historical replacement, "pix-host backup"/"pix-host
// restore", was itself retired inside pix-host to `pix-host memory
// snapshot|restore PATH`; sending a user to a hop that is ALSO a dead end is
// exactly the bug terminalReplacement's pix-host chain-following in
// retired.go fixes), and must not touch anything under HOME.
func TestPixBackupRestore_RetireToLiveHostReplacement(t *testing.T) {
	bin := buildPixBinary(t)
	want := map[string]string{"backup": "pix-host memory snapshot PATH", "restore": "pix-host memory restore PATH"}
	for _, verb := range []string{"backup", "restore"} {
		t.Run(verb, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(bin, verb)
			cmd.Env = append(os.Environ(), "HOME="+dir, "XDG_CONFIG_HOME="+dir, "XDG_STATE_HOME="+dir)
			out, err := cmd.CombinedOutput()
			exitErr, ok := err.(*exec.ExitError)
			if !ok {
				t.Fatalf("pix %s: want a non-zero exit, got err=%v out=%s", verb, err, out)
			}
			if exitErr.ExitCode() != 2 {
				t.Errorf("pix %s exit = %d, want 2", verb, exitErr.ExitCode())
			}
			if !strings.Contains(string(out), "PIX_RETIRED") {
				t.Errorf("pix %s output missing PIX_RETIRED:\n%s", verb, out)
			}
			if !strings.Contains(string(out), want[verb]) {
				t.Errorf("pix %s output = %q, want it to name the live command %q directly (not a second dead end)", verb, out, want[verb])
			}
			if strings.Contains(string(out), "pix-host "+verb+" ") || strings.HasSuffix(strings.TrimSpace(string(out)), "pix-host "+verb+".") {
				t.Errorf("pix %s output still names the retired pix-host hop instead of the live command:\n%s", verb, out)
			}
			entries, walkErr := os.ReadDir(dir)
			if walkErr != nil {
				t.Fatalf("read %s: %v", dir, walkErr)
			}
			if len(entries) != 0 {
				t.Errorf("pix %s created state under HOME: %v", verb, entries)
			}
		})
	}
}

// TestPixHostBackupRestore_FinalHopNamesMemorySnapshot proves the retirement
// CHAIN actually lands somewhere live: `pix-host backup`/`pix-host restore`
// (what `pix backup`/`pix restore` tell the user to run next) must themselves
// answer PIX_RETIRED naming `pix-host memory snapshot`/`memory restore` — a
// user who follows the launcher's advice one more hop must not land on
// another dead end.
func TestPixHostBackupRestore_FinalHopNamesMemorySnapshot(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr != nil {
		t.Fatalf("resolved %s does not look like the services/host module root: %v", root, statErr)
	}
	bin := filepath.Join(t.TempDir(), "pix-host")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, buildErr := build.CombinedOutput(); buildErr != nil {
		t.Fatalf("go build pix-host: %v\n%s", buildErr, out)
	}
	for verb, want := range map[string]string{
		"backup":  "pix-host memory snapshot PATH",
		"restore": "pix-host memory restore PATH",
	} {
		t.Run(verb, func(t *testing.T) {
			cmd := exec.Command(bin, verb)
			cmd.Env = append(os.Environ(), "MEMORY_DB="+filepath.Join(t.TempDir(), "memory.db"))
			out, runErr := cmd.CombinedOutput()
			exitErr, ok := runErr.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 2 {
				t.Fatalf("pix-host %s: err=%v out=%s, want exit 2", verb, runErr, out)
			}
			if !strings.Contains(string(out), want) {
				t.Errorf("pix-host %s = %q, want it to name %q", verb, out, want)
			}
		})
	}
}
