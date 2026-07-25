package main

// rereview_high12_test.go — re-review HIGH findings 1 and 2.
//
//  1: EVERY read-only sbx readiness/state probe on the run/setup/task/
//     secret-sync paths is BOUNDED (probeRun -> runWithTimeout in production):
//     a hanging sbx resolves to unknown/unverifiable and can never wedge a
//     verb. Mutating sbx commands (run/rm/mcp add/load) are lifecycle, not
//     probes, and stay on env.run. The hanging-fake-executable tests reuse
//     hangingProbe (redrive_findings2_test.go), which drives the REAL
//     runWithTimeoutD path under a short injected deadline.
//
//  2: unwrapOpRun accepts ONLY the exact op-run wrapper grammar the launcher
//     generates (mcpRegistrar.execArgv via the shared opRunWrapPrefix):
//     canonical op executable, literal `run`, `--no-masking`,
//     `--env-file=<the launcher's resolved op-refs.env>`, exactly one `--`,
//     then a non-empty trusted inner argv. Arbitrary op subcommands, missing/
//     extra/reordered options, alternate env files, two-token env files, and
//     multiple separators are all rejected — and doctor NEVER probes a
//     rejected registration.

import (
	"bytes"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- finding 1: bounded sbx state probes ------------------------------------

// sbxOnlyLookPath resolves only sbx, mirroring a host where sbx is installed.
func sbxOnlyLookPath(name string) (string, error) {
	if name == "sbx" {
		return "/usr/bin/sbx", nil
	}
	return "", fmt.Errorf("%q not found", name)
}

// TestProbeTaskSandbox_HangingSbxIsUnknownBounded pins the run-preflight and
// task/setup state probe (`pi-stack run`'s create-vs-reattach decision, setup's
// agent-phase gate, task rm/gc guards all route through probeTaskSandbox):
// a wedged `sbx ls` must classify sbxUnknown under the bounded seam, quickly.
func TestProbeTaskSandbox_HangingSbxIsUnknownBounded(t *testing.T) {
	env := shellEnv{
		lookPath: sbxOnlyLookPath,
		probe:    hangingProbe(t, 100*time.Millisecond),
	}
	start := time.Now()
	if st := probeTaskSandbox(env, "pi-stack-x"); st != sbxUnknown {
		t.Errorf("hanging `sbx ls` must classify sbxUnknown, got %v", st)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("probeTaskSandbox took %s — unbounded", el)
	}
}

// TestProbeTaskSandbox_UsesBoundedSeam: when the bounded probe seam is wired
// (defaultShellEnv always wires it), probeTaskSandbox must route through it,
// never the unbounded env.run.
func TestProbeTaskSandbox_UsesBoundedSeam(t *testing.T) {
	env := shellEnv{
		run: func(name string, args ...string) (string, error) {
			t.Fatalf("probeTaskSandbox must use the bounded probe seam, not env.run: %s %v", name, args)
			return "", nil
		},
		probe: func(name string, args ...string) (string, bool, error) {
			if name != "sbx" || len(args) != 1 || args[0] != "ls" {
				return "", false, fmt.Errorf("unexpected probe %s %v", name, args)
			}
			return "pi-stack-x  abc123  running\n", false, nil
		},
	}
	if st := probeTaskSandbox(env, "pi-stack-x"); st != sbxRunning {
		t.Errorf("probe-seam `sbx ls` = %v, want sbxRunning", st)
	}
	if defaultShellEnv().probe == nil {
		t.Fatal("defaultShellEnv must wire the bounded probe seam — production probes would be unbounded")
	}
}

func TestTaskSandboxStatus_HangingSbxIsEmptyBounded(t *testing.T) {
	env := shellEnv{
		lookPath: sbxOnlyLookPath,
		probe:    hangingProbe(t, 100*time.Millisecond),
	}
	start := time.Now()
	if s := taskSandboxStatus(env, "pi-stack-x"); s != "" {
		t.Errorf("hanging `sbx ls` must yield an empty display status, got %q", s)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("taskSandboxStatus took %s — unbounded", el)
	}
}

// TestSetupHandoff_HangingSbxFailsClosed: setup's agent phase probes the
// sandbox state before any handoff; a hanging sbx is sbxUnknown, which must
// FAIL CLOSED (never launch) and must not hang.
func TestSetupHandoff_HangingSbxFailsClosed(t *testing.T) {
	env := shellEnv{
		lookPath: sbxOnlyLookPath,
		probe:    hangingProbe(t, 100*time.Millisecond),
	}
	start := time.Now()
	state := probeTaskSandbox(env, "pi-stack-ws")
	if state != sbxUnknown {
		t.Fatalf("hanging probe must be sbxUnknown, got %v", state)
	}
	var out bytes.Buffer
	err := runSetupHandoff(".", "pi-stack-ws", state, false, &out, func([]string) {
		t.Fatal("setup must never launch on an indeterminate sandbox state")
	})
	if err == nil || !strings.Contains(err.Error(), "cannot determine the state") {
		t.Errorf("setup with an unknown sandbox state must fail closed, got: %v", err)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("setup state path took %s — unbounded", el)
	}
}

// TestLocalImageLoaded_HangingSbxBounded: `pi-stack run --dev`'s image-loaded
// preflight (`sbx template ls`) is read-only; a hang is bounded and degrades
// to the documented fail-open "no signal" answer — never a wedge, and never
// through the unbounded env.run.
func TestLocalImageLoaded_HangingSbxBounded(t *testing.T) {
	env := shellEnv{
		lookPath: sbxOnlyLookPath,
		run: func(name string, args ...string) (string, error) {
			t.Fatalf("localImageLoaded must use the bounded probe seam, not env.run: %s %v", name, args)
			return "", nil
		},
		probe: hangingProbe(t, 100*time.Millisecond),
	}
	start := time.Now()
	if !localImageLoaded(env, "local-12345") {
		t.Error("a timed-out `sbx template ls` is no signal — must fail open (true), never block the launch")
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("localImageLoaded took %s — unbounded", el)
	}
}

// TestEnsureProviderKeysFromRefs_HangingSbxBoundedNoMutation: the secret-sync
// read probe (`sbx secret ls`) is bounded; on a hang the sync returns without
// guessing — it must never reach op or `sbx secret set`.
func TestEnsureProviderKeysFromRefs_HangingSbxBoundedNoMutation(t *testing.T) {
	env := shellEnv{
		lookPath: sbxOnlyLookPath,
		getenv:   func(string) string { return "" },
		homeDir:  func() string { return "/home/u" },
		readFile: func(string) (string, error) {
			return "ANTHROPIC_API_KEY=op://Vault/Anthropic/api key\n", nil
		},
		run: func(name string, args ...string) (string, error) {
			t.Fatalf("a hung `sbx secret ls` must abort the sync before any op/sbx mutation: %s %v", name, args)
			return "", nil
		},
		probe: hangingProbe(t, 100*time.Millisecond),
	}
	var out bytes.Buffer
	start := time.Now()
	ensureProviderKeysFromRefsLocked(env, &out)
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("ensureProviderKeysFromRefsLocked took %s — unbounded", el)
	}
	if strings.Contains(out.String(), "resolved") {
		t.Errorf("a hung probe must not claim any key was resolved, got:\n%s", out.String())
	}
}

// TestBuildTrustedHostState_HangingSbxBounded: setup's trusted host-state
// payload reads `sbx secret ls`; a hang must leave every key un-claimed
// (sbxOK=false semantics) and complete quickly.
func TestBuildTrustedHostState_HangingSbxBounded(t *testing.T) {
	env := shellEnv{
		lookPath: sbxOnlyLookPath,
		getenv:   func(string) string { return "" },
		statFile: func(string) bool { return false },
		readFile: func(string) (string, error) { return "", fmt.Errorf("no file") },
		homeDir:  func() string { return "/home/u" },
		dial:     func(int) bool { return false },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" {
				t.Fatalf("host-state's sbx read must use the bounded probe seam, not env.run: %s %v", name, args)
			}
			return "", fmt.Errorf("no fake output")
		},
		probe: hangingProbe(t, 100*time.Millisecond),
	}
	start := time.Now()
	hs := buildTrustedHostState(defaultCfg(), env, "")
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("buildTrustedHostState took %s — unbounded", el)
	}
	if hs.Keys.Anthropic || hs.Keys.OpenAI || hs.Keys.Google {
		t.Errorf("a hung `sbx secret ls` must never claim a provider key is present: %+v", hs.Keys)
	}
}

// TestOnboardReportReadiness_HangingSbxBounded: onboard's readiness report is
// read-only; a hanging sbx must degrade silently (no false key claims) and
// still print the next step, quickly.
func TestOnboardReportReadiness_HangingSbxBounded(t *testing.T) {
	env := shellEnv{
		lookPath: sbxOnlyLookPath,
		run: func(name string, args ...string) (string, error) {
			t.Fatalf("onboard readiness must use the bounded probe seam, not env.run: %s %v", name, args)
			return "", nil
		},
		probe: hangingProbe(t, 100*time.Millisecond),
	}
	var out bytes.Buffer
	start := time.Now()
	onboardReportReadiness(env, &out)
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("onboardReportReadiness took %s — unbounded", el)
	}
	if strings.Contains(out.String(), "No model provider key set") {
		t.Errorf("an unverifiable sbx read must not claim keys are missing, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Next:") {
		t.Errorf("readiness must still print the next step, got:\n%s", out.String())
	}
}

// --- finding 2: exact op-run wrapper grammar --------------------------------

const (
	f2Refs = "/fake/pi-stack/op-refs.env"
	f2Op   = "/usr/local/bin/op"
	f2Gog  = "/usr/local/bin/gog"
	f2Host = "/usr/local/bin/pi-stack-host"
)

// f2Env is a shellEnv where resolveOpRefs answers f2Refs (via PI_STACK_CONFIG)
// and op/gog/pi-stack-host all resolve canonically.
func f2Env() shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) {
			switch name {
			case "op":
				return f2Op, nil
			case "gog":
				return f2Gog, nil
			case "sbx":
				return "/usr/bin/sbx", nil
			}
			return "", fmt.Errorf("%q not found", name)
		},
		getenv: func(k string) string {
			if k == "PI_STACK_CONFIG" {
				return "/fake/pi-stack/config.toml"
			}
			return ""
		},
		statFile:   func(p string) bool { return p == f2Refs },
		hostBinary: func() (string, error) { return f2Host, nil },
	}
}

// TestUnwrapOpRun_AcceptsOnlyLauncherGrammar is the mutation-style grammar
// test: the canonical launcher-generated wrapper unwraps; every single-token
// deviation is rejected.
func TestUnwrapOpRun_AcceptsOnlyLauncherGrammar(t *testing.T) {
	env := f2Env()
	inner := []string{f2Host, "mcp", "slack"}
	canonical := append(opRunWrapPrefix(f2Op, f2Refs), inner...)

	got, ok := unwrapOpRun(env, canonical)
	if !ok || !reflect.DeepEqual(got, inner) {
		t.Fatalf("the canonical launcher wrapper must unwrap to the inner argv, got (%v,%v)", got, ok)
	}
	// A bare (unwrapped) command passes through untouched.
	if got, ok := unwrapOpRun(env, inner); !ok || !reflect.DeepEqual(got, inner) {
		t.Errorf("a bare command must pass through, got (%v,%v)", got, ok)
	}

	rejects := map[string][]string{
		"arbitrary op subcommand (signin)": append([]string{f2Op, "signin", "--no-masking", "--env-file=" + f2Refs, "--"}, inner...),
		"arbitrary op subcommand (plugin)": append([]string{f2Op, "plugin", "run", "--env-file=" + f2Refs, "--"}, inner...),
		"missing --no-masking":             append([]string{f2Op, "run", "--env-file=" + f2Refs, "--"}, inner...),
		"reordered options":                append([]string{f2Op, "run", "--env-file=" + f2Refs, "--no-masking", "--"}, inner...),
		"alternate env file":               append([]string{f2Op, "run", "--no-masking", "--env-file=/tmp/evil.env", "--"}, inner...),
		"two-token env file":               append([]string{f2Op, "run", "--no-masking", "--env-file", f2Refs, "--"}, inner...),
		"extra option injected":            append([]string{f2Op, "run", "--no-masking", "--env-file=" + f2Refs, "--account", "evil", "--"}, inner...),
		"second separator":                 append(append(opRunWrapPrefix(f2Op, f2Refs), inner...), "--", "evil"),
		"empty inner command":              opRunWrapPrefix(f2Op, f2Refs),
		"foreign argv[0] wrapper":          append([]string{"/tmp/evil", "run", "--no-masking", "--env-file=" + f2Refs, "--"}, inner...),
		"look-alike op path":               append([]string{"/tmp/op", "run", "--no-masking", "--env-file=" + f2Refs, "--"}, inner...),
		"empty argv":                       {},
	}
	for name, argv := range rejects {
		if _, ok := unwrapOpRun(env, argv); ok {
			t.Errorf("%s must be rejected, argv=%v", name, argv)
		}
	}

	// No resolvable launcher refs file -> nothing legitimate to unwrap; the
	// wrapped registration is rejected rather than blessed against an unknown
	// env file.
	noRefs := env
	noRefs.statFile = func(string) bool { return false }
	if _, ok := unwrapOpRun(noRefs, canonical); ok {
		t.Error("an op-wrapped registration must be rejected when the launcher refs file is unresolvable")
	}
}

// TestUnwrapOpRun_MatchesExecArgvGrammar pins the shared-grammar invariant:
// whatever mcpRegistrar.execArgv generates (for pi-stack-host AND gog),
// unwrapOpRun accepts and unwraps back to the bare server command — the
// recognizer can never drift from the generator.
func TestUnwrapOpRun_MatchesExecArgvGrammar(t *testing.T) {
	env := f2Env()
	reg := mcpRegistrar{op: f2Op, opRefs: f2Refs, hostBin: f2Host, gog: f2Gog, account: "you@example.com"}
	for _, name := range []string{"slack", "gog"} {
		wrapped := reg.execArgv(name)
		want := reg.serverCmd(name)
		got, ok := unwrapOpRun(env, wrapped)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Errorf("execArgv(%q)=%v must unwrap to serverCmd=%v, got (%v,%v)", name, wrapped, want, got, ok)
		}
	}
}

// TestTrustedGogSpawn_WrapperGrammar: the gog trust gate accepts the canonical
// generated wrapper and rejects grammar mutants.
func TestTrustedGogSpawn_WrapperGrammar(t *testing.T) {
	env := f2Env()
	inner := []string{f2Gog, "--account", "you@example.com", "--gmail-no-send", "--wrap-untrusted", "--readonly", "mcp", "--allow-tool", "read"}
	canonical := append(opRunWrapPrefix(f2Op, f2Refs), inner...)
	if norm, ok := trustedGogSpawn(env, canonical); !ok || norm[0] != f2Op {
		t.Fatalf("canonical op-wrapped gog spawn must be trusted with canonical tokens, got (%v,%v)", norm, ok)
	}
	rejects := map[string][]string{
		"op signin wrapper":  append([]string{f2Op, "signin", "--no-masking", "--env-file=" + f2Refs, "--"}, inner...),
		"alternate env file": append([]string{f2Op, "run", "--no-masking", "--env-file=/tmp/evil.env", "--"}, inner...),
		"extra option":       append([]string{f2Op, "run", "--no-masking", "--env-file=" + f2Refs, "--evil", "--"}, inner...),
	}
	for name, argv := range rejects {
		if _, ok := trustedGogSpawn(env, argv); ok {
			t.Errorf("%s must not be trusted, argv=%v", name, argv)
		}
	}
}

// TestRecognizedMCPArgv_WrapperGrammar: same gate for pi-stack-host servers.
func TestRecognizedMCPArgv_WrapperGrammar(t *testing.T) {
	env := f2Env()
	inner := []string{f2Host, "mcp", "slack"}
	canonical := append(opRunWrapPrefix(f2Op, f2Refs), inner...)
	norm, ok := recognizedMCPArgv(env, canonical, "slack")
	if !ok || norm[0] != f2Op || norm[len(norm)-3] != f2Host {
		t.Fatalf("canonical op-wrapped host spawn must be recognized with canonical tokens, got (%v,%v)", norm, ok)
	}
	rejects := map[string][]string{
		"op signin wrapper":  append([]string{f2Op, "signin", "--no-masking", "--env-file=" + f2Refs, "--"}, inner...),
		"alternate env file": append([]string{f2Op, "run", "--no-masking", "--env-file=/tmp/evil.env", "--"}, inner...),
		"missing no-masking": append([]string{f2Op, "run", "--env-file=" + f2Refs, "--"}, inner...),
		"double separator":   append(append(opRunWrapPrefix(f2Op, f2Refs), inner...), "--"),
	}
	for name, argv := range rejects {
		if _, ok := recognizedMCPArgv(env, argv, "slack"); ok {
			t.Errorf("%s must not be recognized, argv=%v", name, argv)
		}
	}
}

// TestMcpLocalCheck_RejectedWrapperNeverProbed: doctor reads back a
// registration whose op-run wrapper deviates from the launcher grammar
// (alternate env file). The registration must classify unverifiable and the
// command must NEVER be executed as a probe.
func TestMcpLocalCheck_RejectedWrapperNeverProbed(t *testing.T) {
	env := f2Env()
	reg := f2Op + " run --no-masking --env-file=/tmp/evil.env -- " + f2Host + " mcp slack"
	env.probe = func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if strings.Contains(key, "--list-tools") {
			t.Fatalf("doctor must never probe a rejected registration: %s", key)
		}
		if key == "sbx mcp get slack" {
			return "name: slack\ncommand: " + reg + "\n", false, nil
		}
		return "", false, fmt.Errorf("no fake output for %q", key)
	}
	env.run = func(name string, args ...string) (string, error) {
		t.Fatalf("rejected registration must never be exec'd: %s %v", name, args)
		return "", nil
	}
	c := mcpLocalCheck(env, "slack", "slack\n")
	if c.result() != verdictUnverifiable || !strings.Contains(c.detail, "never executed") {
		t.Errorf("rejected wrapper = %+v, want unverifiable + never-executed note", c)
	}
}

// TestGogParse_RejectedWrapperFallsThrough: an op-wrapped gog registration
// with a non-launcher env file no longer parses as a confident registered
// command, so doctor falls back to the reconstruction — the registered
// command itself is never probed.
func TestGogParse_RejectedWrapperFallsThrough(t *testing.T) {
	env := f2Env()
	line := "name: gog\ncommand: " + f2Op + " run --no-masking --env-file=/tmp/evil.env -- " + f2Gog + " --account you@example.com mcp\n"
	if argv, ok := parseGogCommandLine(env, line); ok {
		t.Errorf("a non-launcher wrapper must not parse as a confident gog command, got %v", argv)
	}
	canonical := "name: gog\ncommand: " + f2Op + " run --no-masking --env-file=" + f2Refs + " -- " + f2Gog + " --account you@example.com mcp\n"
	if _, ok := parseGogCommandLine(env, canonical); !ok {
		t.Error("the canonical launcher wrapper must still parse")
	}
}

var _ = filepath.Clean // keep filepath imported if fixtures above change
