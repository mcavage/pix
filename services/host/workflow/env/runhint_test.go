package env

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

// setRunHintState points config.StateDir at a fresh temp dir for this test,
// the same XDG_STATE_HOME seam every other launcher-state test in this tree
// uses (see workflow/pack's fixture_test.go).
func setRunHintState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
}

func writeSbxenv(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, SbxEnvFileName), []byte(minimalSbxenv), 0o644); err != nil {
		t.Fatal(err)
	}
}

// AC-59: absent `.sbxenv.yaml` -> no hint, ever.
func TestRunHint_AbsentFile_NoHint(t *testing.T) {
	setRunHintState(t)
	ws := t.TempDir()
	got := RunHint(&config.Config{}, ws)
	if got != "" {
		t.Fatalf("RunHint with no .sbxenv.yaml = %q, want empty", got)
	}
}

// AC-59: a present, unregistered `.sbxenv.yaml` on a host with NO registered
// environment prints the negative-first hint naming only `pix env add`.
func TestRunHint_PresentFile_NoRegistration_Hints(t *testing.T) {
	setRunHintState(t)
	ws := t.TempDir()
	writeSbxenv(t, ws)
	got := RunHint(&config.Config{}, ws)
	if got == "" {
		t.Fatal("RunHint with an unregistered .sbxenv.yaml and no registrations = empty, want the hint")
	}
	if !strings.Contains(got, "pix env add") {
		t.Errorf("hint = %q, want it to name `pix env add`", got)
	}
	if strings.Contains(got, "pix env review") || strings.Contains(got, "pix help env") {
		t.Errorf("hint = %q, must name ONLY `pix env add`, no other env command", got)
	}
	// Leads with the negative: the first word after the "pix: " prefix says
	// what did NOT happen, before anything else.
	body := strings.TrimPrefix(got, "pix: ")
	if !strings.HasPrefix(body, "did not select") {
		t.Errorf("hint = %q, must lead with the negative (\"did not select\")", got)
	}
}

// AC-59/AC-60: at least one registered environment anywhere on the host
// suppresses the hint outright, even with an unregistered .sbxenv.yaml
// sitting right here.
func TestRunHint_RegisteredEnvironment_Suppresses(t *testing.T) {
	setRunHintState(t)
	ws := t.TempDir()
	writeSbxenv(t, ws)
	cfg := &config.Config{Environments: map[string]string{"home": "/somewhere"}}
	got := RunHint(cfg, ws)
	if got != "" {
		t.Fatalf("RunHint with a registered environment = %q, want empty (suppressed)", got)
	}
}

// AC-59: the durable marker fires at most once per canonical workspace,
// across separate RunHint calls (modeling separate `pix run` invocations).
func TestRunHint_RepeatedRun_ShowsOnce(t *testing.T) {
	setRunHintState(t)
	ws := t.TempDir()
	writeSbxenv(t, ws)
	cfg := &config.Config{}
	first := RunHint(cfg, ws)
	if first == "" {
		t.Fatal("first RunHint = empty, want the hint")
	}
	second := RunHint(cfg, ws)
	if second != "" {
		t.Fatalf("second RunHint on the same workspace = %q, want empty (already shown)", second)
	}
	third := RunHint(cfg, ws)
	if third != "" {
		t.Fatalf("third RunHint on the same workspace = %q, want empty (still already shown)", third)
	}
}

// AC-59: the marker is scoped per canonical workspace, not global — a
// DIFFERENT workspace with its own .sbxenv.yaml still gets its own hint even
// after another workspace already consumed its once-per-workspace hint.
func TestRunHint_DifferentWorkspaces_EachShowsOnce(t *testing.T) {
	setRunHintState(t)
	wsA := t.TempDir()
	wsB := t.TempDir()
	writeSbxenv(t, wsA)
	writeSbxenv(t, wsB)
	cfg := &config.Config{}

	if got := RunHint(cfg, wsA); got == "" {
		t.Fatal("first RunHint on workspace A = empty, want the hint")
	}
	if got := RunHint(cfg, wsA); got != "" {
		t.Fatalf("second RunHint on workspace A = %q, want empty", got)
	}
	if got := RunHint(cfg, wsB); got == "" {
		t.Fatal("first RunHint on workspace B = empty, want the hint (independent of A's marker)")
	}
	if got := RunHint(cfg, wsB); got != "" {
		t.Fatalf("second RunHint on workspace B = %q, want empty", got)
	}
}

// The marker itself is a real, durable file in launcher-owned state, not an
// in-memory-only flag — a fresh process (a second RunHint call after
// reloading config.StateDir from scratch) must still see it.
func TestRunHint_MarkerIsDurableOnDisk(t *testing.T) {
	setRunHintState(t)
	ws := t.TempDir()
	writeSbxenv(t, ws)
	cfg := &config.Config{}
	if got := RunHint(cfg, ws); got == "" {
		t.Fatal("first RunHint = empty, want the hint")
	}
	dir, err := config.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, runHintStoreName)); err != nil {
		t.Fatalf("marker file not found in state dir: %v", err)
	}
	if got := RunHint(cfg, ws); got != "" {
		t.Fatalf("RunHint after marker persisted = %q, want empty", got)
	}
}
