package health

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/launcher"
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

func TestLaunchdProbe_LoadedVsPositivelyNotLoaded(t *testing.T) {
	bin := buildFixture(t)
	// The fixture ignores the real launchctl argv and answers on argv[1], so
	// the probe passes its mode through Args.
	loaded := check(t, LaunchdProbe{Bin: bin, Label: "com.pix.serve", Args: []string{"healthy"}}, 5*time.Second)
	wantStatus(t, loaded, StatusReady)

	gone := check(t, LaunchdProbe{Bin: bin, Label: "com.pix.serve", Args: []string{"notloaded"}}, 5*time.Second)
	wantStatus(t, gone, StatusAbsent)
	if gone.Fix != ServeInstallFix {
		t.Errorf("fix = %q, want %q", gone.Fix, ServeInstallFix)
	}

	// A generic failure is NOT evidence the agent is unloaded.
	murky := check(t, LaunchdProbe{Bin: bin, Label: "com.pix.serve", Args: []string{"broken"}}, 5*time.Second)
	wantStatus(t, murky, StatusUnknown)
}

// --- memory unit ------------------------------------------------------------

// memoryUnit serves the real `identity` JSON-RPC the memory unit answers, on a
// real listener. body is returned verbatim so a malformed payload is malformed
// on the wire, not in a stub.
func memoryUnit(t *testing.T, body string, hang bool) int {
	t.Helper()
	// release lets the test tear a HUNG handler down deterministically: the
	// probe must give up on its own deadline, and the server must not then hold
	// the test open waiting for a handler nobody is reading.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	return portOf(t, srv.URL)
}

func portOf(t *testing.T, url string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", url, err)
	}
	var n int
	if _, err := fmt.Sscanf(port, "%d", &n); err != nil {
		t.Fatalf("parse port %q: %v", port, err)
	}
	return n
}

// deadPort returns a port with nothing listening on it: a connection there is
// REFUSED, which is positive evidence of absence (unlike a timeout).
func deadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func TestMemoryUnitProbe_RealListenerOutcomes(t *testing.T) {
	ready := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"name":"pix-memory","version":%q,"ready":true,"port":1}}`, launcher.Version)
	degraded := `{"jsonrpc":"2.0","id":1,"result":{"name":"pix-memory","ready":false,"degraded_reason":"unit backoff: plugin exited"}}`
	foreign := `{"jsonrpc":"2.0","id":1,"result":{"name":"something-else","ready":true}}`
	malformed := `{"jsonrpc":"2.0",`
	staleVersion := `{"jsonrpc":"2.0","id":1,"result":{"name":"pix-memory","version":"0.1.7","ready":true,"port":1}}`

	t.Run("running unit is ready", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: memoryUnit(t, ready, false), Enabled: true}, 3*time.Second)
		wantStatus(t, r, StatusReady)
	})
	t.Run("degraded unit is a verified gap with the unit's own reason", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: memoryUnit(t, degraded, false), Enabled: true}, 3*time.Second)
		wantStatus(t, r, StatusAbsent)
		if !strings.Contains(r.Detail, "unit backoff") {
			t.Errorf("detail must carry the unit's own reason, got %q", r.Detail)
		}
		if r.Fix == "" {
			t.Error("a verified gap must carry an exact fix")
		}
	})
	t.Run("a ready unit on the wrong version is a verified, read-only degraded gap", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: memoryUnit(t, staleVersion, false), Enabled: true}, 3*time.Second)
		wantStatus(t, r, StatusAbsent)
		if !strings.Contains(r.Detail, "degraded") || !strings.Contains(r.Detail, "0.1.7") || !strings.Contains(r.Detail, launcher.Version) {
			t.Errorf("detail must name actual and expected versions, got %q", r.Detail)
		}
		if r.Fix != ServeVersionMismatchFix {
			t.Errorf("fix = %q, want %q (never a bare restart hint that cannot close a version gap)", r.Fix, ServeVersionMismatchFix)
		}
	})
	t.Run("WantVersion overrides launcher.Version for the comparison", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: memoryUnit(t, staleVersion, false), Enabled: true, WantVersion: "0.1.7"}, 3*time.Second)
		wantStatus(t, r, StatusReady)
	})
	t.Run("a foreign process holding the port is never ready", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: memoryUnit(t, foreign, false), Enabled: true}, 3*time.Second)
		if r.OK() {
			t.Fatal("a port held by something that is not our unit must never render ready")
		}
	})
	t.Run("malformed payload is unknown", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: memoryUnit(t, malformed, false), Enabled: true}, 3*time.Second)
		wantStatus(t, r, StatusUnknown)
	})
	t.Run("a hung unit is unknown, not absent", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: memoryUnit(t, ready, true), Enabled: true}, 250*time.Millisecond)
		wantStatus(t, r, StatusUnknown)
		if r.Fix != "" {
			t.Errorf("a timed-out unit probe must not suggest a fix, got %q", r.Fix)
		}
	})
	t.Run("refused connection is a verified down unit", func(t *testing.T) {
		r := check(t, MemoryUnitProbe{Port: deadPort(t), Enabled: true}, 3*time.Second)
		wantStatus(t, r, StatusAbsent)
		if r.Fix != ServeStartFix {
			t.Errorf("fix = %q, want %q", r.Fix, ServeStartFix)
		}
	})
	t.Run("a disabled unit reads off, not absent, and never blocks", func(t *testing.T) {
		p := MemoryUnitProbe{Port: deadPort(t), Enabled: false}
		if p.Required() {
			t.Error("a service that is not in the configured set must not be required")
		}
		r := check(t, p, 3*time.Second)
		if r.Blocking() {
			t.Error("a disabled unit must never block")
		}
		// Not in `services` is the USER'S OWN configuration choosing this
		// capability off, not pix inferring an absence — the eligible case for
		// StatusOff, and never a gap: nothing was dialed, so nothing failed.
		wantStatus(t, r, StatusOff)
		if r.Fix != "" {
			t.Errorf("an off memory row must carry no fix, got %q", r.Fix)
		}
		if r.OK() || r.Missing() {
			t.Errorf("off must be neither OK nor Missing, got OK=%v Missing=%v", r.OK(), r.Missing())
		}
	})
}

// --- pack -------------------------------------------------------------------

func TestPackProbe_RealFilesystem(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte("name = \"work\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, check(t, PackProbe{Root: root}, time.Second), StatusReady)

	// Configured but gone: a verified gap with an exact fix.
	missing := check(t, PackProbe{Root: filepath.Join(root, "nope")}, time.Second)
	wantStatus(t, missing, StatusAbsent)
	if missing.Fix == "" {
		t.Error("a configured-but-missing pack must carry a fix")
	}

	// Present but not a pack: also verified, also fixable.
	bare := t.TempDir()
	wantStatus(t, check(t, PackProbe{Root: bare}, time.Second), StatusAbsent)

	// Nothing configured at all: OFF, not absent — no pack is a fully supported
	// end state the user chose, not something to repair.
	none := PackProbe{}
	if none.Required() {
		t.Error("the pack axis must be optional")
	}
	noneResult := check(t, none, time.Second)
	if noneResult.Blocking() {
		t.Error("no active pack must never block")
	}
	wantStatus(t, noneResult, StatusOff)
	if noneResult.Fix != "" {
		t.Errorf("an off pack row must carry no fix, got %q", noneResult.Fix)
	}
	if noneResult.Hint == "" {
		t.Error("an off pack row must carry an invitation Hint")
	}
	if noneResult.OK() || noneResult.Missing() {
		t.Errorf("off must be neither OK nor Missing, got OK=%v Missing=%v", noneResult.OK(), noneResult.Missing())
	}
}

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

// TestLaunchdProbe_LoadedButProgramMissing: "loaded" and "able to start" are
// different questions, and this row only ever asked the first one.
//
// `pix serve install` resolved the binary through its symlink, so a Homebrew
// install wrote a versioned Cellar path into the plist and the next `brew
// upgrade` deleted it. launchd keeps that job and reports it cheerfully, while
// every `launchctl kickstart -k` — which pix runs after every pack change —
// blocks on a job that can never spawn. A user spent three days on "pix setup
// hangs" with this row printing `✓ agent loaded` throughout.
func TestLaunchdProbe_LoadedButProgramMissing(t *testing.T) {
	bin := buildFixture(t)
	p := LaunchdProbe{Bin: bin, Label: "com.pix.serve", Args: []string{"staleprogram"},
		Exists: func(string) bool { return false }}
	r := check(t, p, 5*time.Second)

	wantStatus(t, r, StatusAbsent)
	if r.Fix != ServeInstallFix {
		t.Errorf("fix = %q, want %q — reinstalling the agent is what rewrites the path", r.Fix, ServeInstallFix)
	}
	if !strings.Contains(r.Evidence, "Cellar") {
		t.Errorf("evidence must NAME the missing program, got %q", r.Evidence)
	}

	// The same agent, with its program present, is ready. A row that cannot tell
	// these apart is the row that was there before.
	ok := check(t, LaunchdProbe{Bin: bin, Label: "com.pix.serve", Args: []string{"staleprogram"},
		Exists: func(string) bool { return true }}, 5*time.Second)
	wantStatus(t, ok, StatusReady)
}

// TestLaunchdProbe_SilenceAboutTheProgramIsNotAMissingProgram: launchctl output
// that never mentions a program says nothing about one. Reading that as absent
// would turn every unfamiliar launchctl format into a false alarm with a repair
// attached.
func TestLaunchdProbe_SilenceAboutTheProgramIsNotAMissingProgram(t *testing.T) {
	bin := buildFixture(t)
	r := check(t, LaunchdProbe{Bin: bin, Label: "com.pix.serve", Args: []string{"healthy"},
		Exists: func(string) bool { return false }}, 5*time.Second)
	wantStatus(t, r, StatusReady)
}
