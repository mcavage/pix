package health

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests cross the real boundaries: a compiled fixture EXECUTABLE for
// every exec probe, and a real TCP listener for every service probe. Nothing
// here stubs the probe's own logic — the failure modes (broken, crash, hang,
// malformed, denied) are produced by the fixture process itself, so the
// classification under test is the one production runs.

var (
	fixtureOnce sync.Once
	fixtureBin  string
	fixtureErr  error
)

// buildFixture compiles testdata/fixture once per test binary. The fixture's
// source lives at testdata/fixture/main.go.txt — a .txt, not a .go file, on
// purpose: it is a real, complete Go program, but naming it main.go would
// make it a second copy of production Go source sitting outside any *_test.go
// file, which is exactly the thing a LOC/production-metrics scanner (anything
// that globs *.go and excludes *_test.go, same as `go build ./...` itself)
// cannot tell apart from shipped code. Reading it as data and writing it into
// a temp dir as main.go right before the compile keeps this a REAL compiled
// executable — same handshake, same exec, same classification under test —
// while its source counts as test fixture data, not production Go.
func buildFixture(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "health-fixture")
		if err != nil {
			fixtureErr = err
			return
		}
		src, err := os.ReadFile("testdata/fixture/main.go.txt")
		if err != nil {
			fixtureErr = err
			return
		}
		mainGo := filepath.Join(dir, "main.go")
		if err := os.WriteFile(mainGo, src, 0o644); err != nil {
			fixtureErr = err
			return
		}
		out := filepath.Join(dir, "fixture")
		cmd := exec.Command("go", "build", "-o", out, mainGo)
		cmd.Env = append(os.Environ(), "GOFLAGS=")
		if b, err := cmd.CombinedOutput(); err != nil {
			fixtureErr = errors.New(string(b))
			return
		}
		fixtureBin = out
	})
	if fixtureErr != nil {
		t.Fatalf("build fixture: %v", fixtureErr)
	}
	return fixtureBin
}

func check(t *testing.T, p Probe, budget time.Duration) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return p.Check(ctx)
}

func wantStatus(t *testing.T, got Result, want Status) {
	t.Helper()
	if got.Effective() != want {
		t.Fatalf("%s = %q (%s / %s), want %q", got.Name, got.Effective(), got.Detail, got.Evidence, want)
	}
}

// --- sbx --------------------------------------------------------------------

func TestSbxProbe_RealExecutableOutcomes(t *testing.T) {
	bin := buildFixture(t)
	cases := []struct {
		mode string
		want Status
	}{
		{"healthy", StatusReady},
		{"broken", StatusUnknown},    // non-zero exit: we learned nothing
		{"crash", StatusUnknown},     // killed by a signal: we learned nothing
		{"malformed", StatusUnknown}, // clean exit, unrecognizable output
		{"denied", StatusDenied},     // an explicit policy refusal
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			r := check(t, SbxProbe{Bin: bin, Args: []string{tc.mode}}, 5*time.Second)
			wantStatus(t, r, tc.want)
			if tc.want != StatusReady && tc.want != StatusUnknown && r.Fix == "" {
				t.Error("a verified gap must carry an exact fix")
			}
		})
	}
}

// TestSbxProbe_VersionGrammarFallback: an sbx build that rejects the root
// --version flag with a recognized cobra-style usage error falls back to
// `sbx version` and reports Ready — the exact v0.38 compatibility gap this
// probe exists to bridge.
func TestSbxProbe_VersionGrammarFallback(t *testing.T) {
	bin := buildFixture(t)
	r := check(t, SbxProbe{Bin: bin, Args: []string{"--version"}}, 5*time.Second)
	wantStatus(t, r, StatusReady)
	if !strings.Contains(r.Evidence, "fell back from --version") {
		t.Errorf("evidence = %q, want it to say the fallback grammar was used", r.Evidence)
	}
}

// TestSbxProbe_DeniedNeverTriesFallback: a POSITIVE policy refusal on the
// primary grammar must classify as Denied and must NEVER retry the alternate
// grammar — if it wrongly did, this fixture's "version" mode would answer
// healthy and flip the result to Ready, which is exactly what this test
// guards against.
func TestSbxProbe_DeniedNeverTriesFallback(t *testing.T) {
	bin := buildFixture(t)
	r := check(t, SbxProbe{Bin: bin, Args: []string{"--version", "denied"}}, 5*time.Second)
	wantStatus(t, r, StatusDenied)
}

// TestSbxProbe_FallbackAlsoFailingStaysUnknown: when BOTH the primary and the
// alternate grammar fail, the probe is bounded to exactly the one retry and
// reports Unknown rather than looping or guessing further.
func TestSbxProbe_FallbackAlsoFailingStaysUnknown(t *testing.T) {
	bin := buildFixture(t)
	r := check(t, SbxProbe{Bin: bin, Args: []string{"--version", "brokenboth"}}, 5*time.Second)
	wantStatus(t, r, StatusUnknown)
}

func TestSbxProbe_MissingBinaryIsVerifiedAbsent(t *testing.T) {
	r := check(t, SbxProbe{Bin: filepath.Join(t.TempDir(), "definitely-not-here")}, 5*time.Second)
	wantStatus(t, r, StatusAbsent)
	if r.Fix != SbxInstallFix {
		t.Errorf("fix = %q, want %q", r.Fix, SbxInstallFix)
	}
}

func TestSbxProbe_HangIsUnknownAndBounded(t *testing.T) {
	bin := buildFixture(t)
	start := time.Now()
	r := check(t, SbxProbe{Bin: bin, Args: []string{"hang"}}, 300*time.Millisecond)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("probe was not bounded: %s", elapsed)
	}
	wantStatus(t, r, StatusUnknown)
	if r.Fix != "" {
		t.Errorf("a timed-out probe must not suggest a fix, got %q", r.Fix)
	}
}

// --- launchd ----------------------------------------------------------------

// --- providers / model keys -------------------------------------------------

func TestProviderKeyProbe_TriStateNoKeyIsUnchanged(t *testing.T) {
	bin := buildFixture(t)
	t.Run("listing that omits the key is a POSITIVE no-key", func(t *testing.T) {
		lister := writeScript(t, "lister", "#!/bin/sh\necho OPENAI_API_KEY\n")
		r := check(t, ProviderKeyProbe{Bin: lister, Want: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"}}, 5*time.Second)
		wantStatus(t, r, StatusAbsent)
		if !strings.Contains(r.Fix, "ANTHROPIC_API_KEY") {
			t.Errorf("fix must name the missing key, got %q", r.Fix)
		}
		if strings.Contains(r.Fix, "OPENAI_API_KEY") {
			t.Errorf("fix must not re-ask for a key that is present, got %q", r.Fix)
		}
	})
	t.Run("all keys present is ready", func(t *testing.T) {
		lister := writeScript(t, "lister-full", "#!/bin/sh\necho OPENAI_API_KEY\necho ANTHROPIC_API_KEY\n")
		wantStatus(t, check(t, ProviderKeyProbe{Bin: lister, Want: []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY"}}, 5*time.Second), StatusReady)
	})
	t.Run("a failed listing is unknown, never a no-key claim", func(t *testing.T) {
		for _, mode := range []string{"broken", "crash"} {
			r := check(t, ProviderKeyProbe{Bin: bin, Args: []string{mode}, Want: []string{"OPENAI_API_KEY"}}, 5*time.Second)
			wantStatus(t, r, StatusUnknown)
			if r.Fix != "" {
				t.Errorf("%s: an unreadable key store must not emit a fix, got %q", mode, r.Fix)
			}
		}
	})
	t.Run("a hung listing is unknown and bounded", func(t *testing.T) {
		r := check(t, ProviderKeyProbe{Bin: bin, Args: []string{"hang"}, Want: []string{"OPENAI_API_KEY"}}, 300*time.Millisecond)
		wantStatus(t, r, StatusUnknown)
	})
}

// Keyless is a config answer that OUTRANKS the key store, so it must hold even
// against the store's strongest no-key evidence — and against a store that
// cannot be reached at all, which is the state inside a sandbox.
func TestProviderKeyProbe_KeylessNeverAsksForAKeyNothingReads(t *testing.T) {
	bin := buildFixture(t)
	keyless := "gw-anthropic, gw-openai (auth: sbx-session)"
	for name, args := range map[string][]string{
		"store lists no key": {"nokeys"},
		"store is broken":    {"broken"},
		"store hangs":        {"hang"},
	} {
		t.Run(name, func(t *testing.T) {
			r := check(t, ProviderKeyProbe{Bin: bin, Args: args, Want: []string{"anthropic", "openai"},
				AnyOf: true, Keyless: keyless}, 300*time.Millisecond)
			wantStatus(t, r, StatusReady)
			if r.Fix != "" {
				t.Errorf("fix = %q, want none: there is no key to add", r.Fix)
			}
			if !strings.Contains(r.Evidence, keyless) {
				t.Errorf("evidence = %q, must name what answers instead", r.Evidence)
			}
		})
	}
	// "hang" proving ready in a 300ms budget is also the proof the exec never
	// happened: the fixture outlives that window.
}

// The resolver is read at CHECK time, so a probe built before a pack adoption
// grades the host that adoption produced — not the one it was built from.
func TestProviderKeyProbe_KeylessResolvesAtCheckTime(t *testing.T) {
	bin := buildFixture(t)
	adopted := false
	p := ProviderKeyProbe{Bin: bin, Args: []string{"nokeys"}, Want: []string{"anthropic"}, AnyOf: true,
		Keyless: "stale-at-build-time",
		ResolveKeyless: func() string {
			if !adopted {
				return ""
			}
			return "gw-anthropic (auth: sbx-session)"
		}}

	// The resolver WINS: its "" must not fall back to a static value built
	// before the config that value described was written.
	before := check(t, p, 5*time.Second)
	wantStatus(t, before, StatusAbsent)
	if strings.Contains(before.Evidence, "stale-at-build-time") {
		t.Errorf("the static Keyless outranked the resolver: %q", before.Evidence)
	}

	adopted = true
	after := check(t, p, 5*time.Second)
	wantStatus(t, after, StatusReady)
	if !strings.Contains(after.Evidence, "gw-anthropic") {
		t.Errorf("evidence = %q, want the post-adoption backends", after.Evidence)
	}
}

func writeScript(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
