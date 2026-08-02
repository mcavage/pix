package main

// gog_setup_hardening_test.go ports the prior branch's review-round guards
// for `pix gworkspace setup` onto the S07 integrated framework: read-only OAuth
// enforced at grant time (no unsafe route fallback), the selected route's OWN
// subcommand flags verified before exec, the bare headless probe when
// op/op-refs are unavailable (zero tools fails; timeout/exec-error are
// unverifiable, never success), sbx required with zero config drift,
// save-failure rollback to the exact prior registration snapshot, the
// tri-state prior-registration snapshot (unknown is never absent), and every
// noninteractive probe bounded.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// --- R1-02: read-only enforced at grant time, no unsafe fallback ---

// TestGogSetup_R102_EveryRouteRequestsReadonlyAtGrantTime is the "tests must
// assert no auth route runs without readonly" requirement: for each of the
// three supported routes, every INTERACTIVE call gogSetup makes is inspected,
// and the one(s) that actually perform an OAuth grant must carry --readonly.
func TestGogSetup_R102_EveryRouteRequestsReadonlyAtGrantTime(t *testing.T) {
	cases := []struct {
		route   string
		help    string
		nCalls  int
		grantIx []int // indices into calls that are OAuth-granting steps
	}{
		{"setup", gogAuthHelpCurrentSetup, 1, []int{0}},
		{"credentials+add", gogAuthHelpCurrentTwoStep, 2, []int{1}},
		{"add-client+login (legacy)", gogAuthHelpLegacy, 2, []int{1}},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			gogSetupTestCfg(t)
			cred := gogCredFile(t)
			var calls [][]string
			ge := gogTestEnv{
				present: map[string]bool{"gog": true, "sbx": true},
				output: mergeOutputs(gogAuthCapabilityFixtures(tc.route, true), gogBareHeadlessFixture("you@example.com"), map[string]string{
					"gog auth --help": tc.help,
					"gog --account you@example.com auth doctor --check": "ok",
				}),
				statFile:      map[string]bool{cred: true},
				interCalls:    &calls,
				sbxRegisterOK: true,
			}
			var out bytes.Buffer
			opts := gogSetupOpts{account: "you@example.com", credentials: cred}
			if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err != nil {
				t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
			}
			if len(calls) != tc.nCalls {
				t.Fatalf("expected %d interactive calls, got %d: %v", tc.nCalls, len(calls), calls)
			}
			for _, ix := range tc.grantIx {
				if !containsToken(calls[ix], "--readonly") {
					t.Errorf("route %q step %d (%v) must request --readonly, it did not", tc.route, ix, calls[ix])
				}
			}
		})
	}
}

func containsToken(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestGogSetup_R102_MissingReadonlyCapability_FailsWithoutFallback covers
// every route's installed CLI failing to advertise --readonly on its
// OAuth-granting step: gogSetup must refuse to run ANY interactive command
// (never silently authorizing without read-only scopes, and never trying a
// different, possibly-unsafe route as a fallback), and the guidance must name
// the installed version and suggest an upgrade.
func TestGogSetup_R102_MissingReadonlyCapability_FailsWithoutFallback(t *testing.T) {
	cases := []struct {
		route string
		help  string
	}{
		{"setup", gogAuthHelpCurrentSetup},
		{"credentials+add", gogAuthHelpCurrentTwoStep},
		{"add-client+login (legacy)", gogAuthHelpLegacy},
	}
	for _, tc := range cases {
		t.Run(tc.route, func(t *testing.T) {
			gogSetupTestCfg(t)
			cred := gogCredFile(t)
			var calls [][]string
			ge := gogTestEnv{
				present: map[string]bool{"gog": true, "sbx": true},
				output: mergeOutputs(gogAuthCapabilityFixtures(tc.route, false /* NO readonly */), map[string]string{
					"gog auth --help": tc.help,
					"gog --version":   "gog version 1.2.3",
				}),
				statFile:   map[string]bool{cred: true},
				interCalls: &calls,
			}
			var out bytes.Buffer
			opts := gogSetupOpts{account: "you@example.com", credentials: cred}
			err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
			if err == nil {
				t.Fatalf("expected an error when the installed gog cannot guarantee read-only scopes for route %q", tc.route)
			}
			if len(calls) != 0 {
				t.Errorf("must not run ANY interactive auth command when read-only cannot be guaranteed, got %v", calls)
			}
			if !strings.Contains(err.Error(), "read-only") {
				t.Errorf("expected a read-only-specific error, got %q", err)
			}
			if !strings.Contains(out.String(), "1.2.3") {
				t.Errorf("expected the installed version in guidance, got %q", out.String())
			}
			if !strings.Contains(out.String(), "upgrade") {
				t.Errorf("expected upgrade guidance, got %q", out.String())
			}
			if !strings.Contains(out.String(), "never falls back") {
				t.Errorf("expected explicit no-fallback wording, got %q", out.String())
			}
		})
	}
}

// --- R1-12: exact subcommand flags, not just top-level names ---

// TestGogSetup_R112_SelectedRouteSubcommandSyntaxChanged_FailsBeforeExec
// covers a gog release that still lists "setup" in the top-level `gog auth
// --help` (so chooseGogAuthRoute's name-based pick still selects it), but
// whose OWN `gog auth setup --help` no longer advertises --credentials/
// --login (the syntax changed underneath the same subcommand name). This
// must be rejected with installed-version guidance BEFORE any auth command
// runs, not discovered as a runtime exec failure.
func TestGogSetup_R112_SelectedRouteSubcommandSyntaxChanged_FailsBeforeExec(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			// The subcommand's OWN help no longer carries --credentials/--login
			// at all (not just missing --readonly) — a syntax change under an
			// unchanged top-level name.
			"gog auth setup --help": "usage: gog auth setup <account>\n\n(no flags)\n",
			"gog --version":         "gog version 2.0.0",
		},
		statFile:   map[string]bool{cred: true},
		interCalls: &calls,
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the selected route's subcommand syntax changed")
	}
	if len(calls) != 0 {
		t.Errorf("must not exec any command once subcommand flags fail to verify, got %v", calls)
	}
	if !strings.Contains(out.String(), "2.0.0") {
		t.Errorf("expected the installed version in guidance, got %q", out.String())
	}
	if !strings.Contains(out.String(), "--credentials") {
		t.Errorf("expected the missing flag to be named in guidance, got %q", out.String())
	}
}

// --- R1-06: bare headless probe when op/op-refs are unavailable ---

// gogR106Env builds a minimal, healthy-up-to-headless-verification gogSetup
// environment with NO op and NO sbx wired as present (mirroring "macOS system
// keychain, no 1Password installed" — R1-06 is explicit that this is not an
// excuse to skip the direct bare probe). Callers add sbx/registration
// fixtures if their case needs to reach registration.
func gogR106Env(t *testing.T, probe func(name string, args ...string) (string, bool, error)) (hostenv.Env, string) {
	t.Helper()
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		statFile:      map[string]bool{cred: true},
		sbxRegisterOK: true,
	}
	env := ge.env()
	// R2-03: snapshotGogRegistration's presence probe (`sbx mcp ls`) defaults
	// to an empty (confirmed-absent) listing unless the caller's probe func
	// itself cares to override it — none of gogR106Env's callers are testing
	// prior-registration behavior, so they'd otherwise have to fixture this
	// unrelated call themselves.
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		if name == "sbx" && len(args) == 2 && args[0] == "mcp" && args[1] == "ls" {
			return "", false, nil
		}
		return probe(name, args...)
	}
	return env, cred
}

// TestGogSetup_R106_BareProbe_ZeroTools_Fails: no op installed at all (op
// absent from lookPath), so the ONLY possible verification is the bare
// headless probe — a clean, zero-tool result must still fail gogSetup with
// the keyring guidance, exactly as the op-wrapped path does.
func TestGogSetup_R106_BareProbe_ZeroTools_Fails(t *testing.T) {
	env, cred := gogR106Env(t, func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		switch key {
		case "gog auth --help":
			return gogAuthHelpCurrentSetup, false, nil
		case "gog auth setup --help":
			return gogAuthSetupHelpReadonly, false, nil
		case "gog --account you@example.com auth doctor --check":
			return "ok", false, nil
		case gogBareHeadlessKey("you@example.com"):
			return "", false, nil // clean exit, ZERO tools
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	})
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the bare headless probe returns zero tools")
	}
	if !strings.Contains(err.Error(), "authorization succeeded but the headless tool listing returned 0 tools; nothing was registered") {
		t.Errorf("expected the zero-tools failure wording, got %q", err)
	}
	if !strings.Contains(out.String(), "GOG_KEYRING_PASSWORD") {
		t.Errorf("expected the keyring guidance, got %q", out.String())
	}
	if strings.Contains(out.String(), "headless tools OK") {
		t.Errorf("must never claim success on a zero-tools result, got %q", out.String())
	}
}

// TestGogSetup_R106_BareProbe_Timeout_UnverifiableNeverSuccess: the bare
// headless probe times out. This is unverifiable, not a confirmed failure of
// a specific kind, but it must NEVER be reported as success, and gogSetup
// must not proceed to register.
func TestGogSetup_R106_BareProbe_Timeout_UnverifiableNeverSuccess(t *testing.T) {
	env, cred := gogR106Env(t, func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		switch key {
		case "gog auth --help":
			return gogAuthHelpCurrentSetup, false, nil
		case "gog auth setup --help":
			return gogAuthSetupHelpReadonly, false, nil
		case "gog --account you@example.com auth doctor --check":
			return "ok", false, nil
		case gogBareHeadlessKey("you@example.com"):
			return "", true, fmt.Errorf("context deadline exceeded")
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	})
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the bare headless probe times out")
	}
	if strings.Contains(out.String(), "headless tools OK") {
		t.Errorf("a timed-out probe must never be reported as success, got %q", out.String())
	}
	if strings.Contains(err.Error(), "0 tools") || strings.Contains(out.String(), "GOG_KEYRING_PASSWORD") {
		t.Errorf("a timeout is unverifiable, not the keyring/zero-tools failure, got err=%q out=%q", err, out.String())
	}
	cfg, _ := config.Load()
	if cfg != nil {
		for _, m := range cfg.MCP {
			if m == gwServerName {
				t.Errorf("must not register gog when headless verification is unverifiable")
			}
		}
	}
}

// TestGogSetup_R106_BareProbe_ExecError_UnverifiableNeverSuccess: the bare
// probe fails to exec (not a timeout, e.g. gog vanished between the earlier
// lookPath and this call) — also unverifiable, never claimed as success.
func TestGogSetup_R106_BareProbe_ExecError_UnverifiableNeverSuccess(t *testing.T) {
	env, cred := gogR106Env(t, func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		switch key {
		case "gog auth --help":
			return gogAuthHelpCurrentSetup, false, nil
		case "gog auth setup --help":
			return gogAuthSetupHelpReadonly, false, nil
		case "gog --account you@example.com auth doctor --check":
			return "ok", false, nil
		case gogBareHeadlessKey("you@example.com"):
			return "", false, fmt.Errorf("exec: not found")
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	})
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the bare headless probe exec-fails")
	}
	if strings.Contains(out.String(), "headless tools OK") {
		t.Errorf("an exec error must never be reported as success, got %q", out.String())
	}
}

// TestGogSetup_R106_BareProbe_Success_RegistersNormally: no op at all, but
// the bare probe cleanly returns tools — gogSetup must succeed and register,
// proving R1-06 doesn't turn a genuinely healthy no-1Password setup into a
// false failure.
func TestGogSetup_R106_BareProbe_Success_RegistersNormally(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		statFile:      map[string]bool{cred: true},
		sbxRegisterOK: true,
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "headless tools OK") {
		t.Errorf("expected the healthy wording, got %q", out.String())
	}
}

// --- R1-08: sbx required, no config/registration drift ---

// TestGogSetup_R108_SbxMissing_FailsAndConfigUnchanged: sbx absent must FAIL
// gogSetup (never a silent "would register" success), and must never touch
// the persisted config.
func TestGogSetup_R108_SbxMissing_FailsAndConfigUnchanged(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	// Pre-seed config with an unrelated prior entry so we can prove it is
	// byte-for-byte untouched, not just that gog is absent.
	pre, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pre.AddMCP("slack")
	if err := pre.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	ge := gogTestEnv{
		present: map[string]bool{"gog": true}, // NO sbx
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err = gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when sbx is not on PATH")
	}
	if !strings.Contains(err.Error(), "sbx") {
		t.Errorf("expected an sbx-specific error, got %q", err)
	}
	after, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config must be byte-for-byte unchanged when sbx is missing:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestGogSetup_R108_RegistrationFails_ConfigUnchanged: sbx IS present, but
// the actual `sbx mcp add google-workspace ...` call fails — config must still be left
// completely unchanged (registration is required BEFORE config is saved).
func TestGogSetup_R108_RegistrationFails_ConfigUnchanged(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	pre, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pre.AddMCP("slack")
	if err := pre.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
			// deliberately NO "sbx mcp add google-workspace ..." fixture and sbxRegisterOK
			// left false, so mcp.RegisterServers' env.Run call for it errors.
		}),
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err = gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when sbx mcp add fails")
	}
	after, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config must be byte-for-byte unchanged when registration fails:\nbefore=%q\nafter=%q", before, after)
	}
}

// gogR108RollbackEnv wires a gogTestEnv plus a run() override that records
// every `sbx mcp add`/`sbx mcp rm` call (so rollback can be asserted
// precisely) and forces `config.Save()` (but NOT config.Load(), which must
// still succeed first) to fail deterministically: the config directory is
// created normally (so Load() sees a not-yet-existing config.toml and returns
// clean defaults), then chmod'd read-only so Save()'s os.CreateTemp inside it
// fails with a permission error. Non-root is guaranteed in this sandbox (see
// AGENTS.md), so the permission bits are honored deterministically.
func gogR108RollbackEnv(t *testing.T, priorRegistered bool) (env hostenv.Env, cred string, addCalls, rmCalls *[][]string) {
	t.Helper()
	dir := t.TempDir()
	cred = gogCredFile(t)
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	fixtures := map[string]string{
		"gog auth --help": gogAuthHelpCurrentSetup,
		"gog --account you@example.com auth doctor --check": "ok",
	}
	priorArgv := []string{"/usr/bin/gog", "--account", "old@example.com", "--gmail-no-send",
		"--wrap-untrusted", "--readonly", "mcp", "--allow-tool", "read"}
	if priorRegistered {
		// R2-03: snapshotGogRegistration confirms presence via the bounded PLAIN
		// `sbx mcp ls` listing FIRST, independent of whether the detailed `sbx
		// mcp get google-workspace` command parses — both must agree gog is registered.
		fixtures["sbx mcp ls"] = "google-workspace\n"
		fixtures["sbx mcp get google-workspace"] = "name: gog\ncommand: " + strings.Join(priorArgv, " ") + "\n"
	}
	ge := gogTestEnv{
		present:  map[string]bool{"gog": true, "sbx": true},
		output:   mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), fixtures),
		statFile: map[string]bool{cred: true},
	}
	e := ge.env()
	baseRun := systest.Of(e.System).RunFn
	addCalls, rmCalls = &[][]string{}, &[][]string{}
	systest.Of(e.System).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" {
			switch args[1] {
			case "add":
				*addCalls = append(*addCalls, append([]string{}, args...))
				return "", nil
			case "rm":
				*rmCalls = append(*rmCalls, append([]string{}, args...))
				return "", nil
			}
		}
		return baseRun(name, args...)
	}
	return e, cred, addCalls, rmCalls
}

// TestGogSetup_R108_SaveFailure_RestoresPriorRegistration: registration
// succeeds (overwriting an EXISTING prior gog registration), but the
// subsequent config.Save() fails — gogSetup must roll the sbx side back to
// exactly the prior registration, and say so explicitly in the error.
func TestGogSetup_R108_SaveFailure_RestoresPriorRegistration(t *testing.T) {
	env, cred, addCalls, rmCalls := gogR108RollbackEnv(t, true)
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when config.Save() fails after a successful registration")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("expected explicit rollback confirmation in the error, got %q", err)
	}
	if len(*rmCalls) != 0 {
		t.Errorf("must restore the prior registration, not just remove the new one, got rm calls %v", *rmCalls)
	}
	if len(*addCalls) != 2 {
		t.Fatalf("expected 2 `sbx mcp add` calls (new registration, then rollback restore), got %d: %v", len(*addCalls), *addCalls)
	}
	if !strings.Contains(strings.Join((*addCalls)[0], " "), "you@example.com") {
		t.Errorf("expected the first add call to register the NEW account, got %v", (*addCalls)[0])
	}
	if !strings.Contains(strings.Join((*addCalls)[1], " "), "old@example.com") {
		t.Errorf("expected the rollback add call to restore the PRIOR command verbatim, got %v", (*addCalls)[1])
	}
}

// TestGogSetup_R108_SaveFailure_RemovesNewRegistrationWhenNoPrior: same as
// above, but there was NO prior gog registration — rollback must REMOVE the
// just-added registration (`sbx mcp rm google-workspace`), not try to restore nothing.
func TestGogSetup_R108_SaveFailure_RemovesNewRegistrationWhenNoPrior(t *testing.T) {
	env, cred, addCalls, rmCalls := gogR108RollbackEnv(t, false)
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when config.Save() fails after a successful registration")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("expected explicit rollback confirmation in the error, got %q", err)
	}
	if len(*addCalls) != 1 {
		t.Fatalf("expected exactly 1 `sbx mcp add` call (the new registration), got %d: %v", len(*addCalls), *addCalls)
	}
	if len(*rmCalls) != 1 || len((*rmCalls)[0]) < 3 || (*rmCalls)[0][2] != gwServerName {
		t.Fatalf("expected a rollback `sbx mcp rm google-workspace` call, got %v", *rmCalls)
	}
}

// --- R1-14 (gog half): the new probes are bounded too ---

// TestGogSetup_R114_CapabilityAndBareHeadlessProbesAreBounded proves the NEW
// probes this round introduces — the per-step capability-help probe
// (gogAuthRouteCapable) and the bare headless probe (R1-06) — go through the
// bounded probe machinery (env.probe), never raw env.Run, exactly like the
// pre-existing help/version/auth-doctor probes.
func TestGogSetup_R114_CapabilityAndBareHeadlessProbesAreBounded(t *testing.T) {
	const acct = "you@example.com"
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	helpKey := "gog auth --help"
	setupHelpKey := "gog auth setup --help"
	authKey := "gog --account " + acct + " auth doctor --check"
	headlessKey := gogBareHeadlessKey(acct)

	var probed []string
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "gog" || name == "sbx" {
			return "/usr/bin/" + name, nil
		}
		return "", fmt.Errorf("exec: %q not found", name)
	}, IsFileFn: func(path string) bool { return path == cred }, ModeFn: func(path string) (os.FileMode, bool) {
		if path == cred {
			return 0o600, true
		}
		return 0, false
	}, GetenvFn: func(string) string { return "" }, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		probed = append(probed, key)
		switch key {
		case helpKey:
			return gogAuthHelpCurrentSetup, false, nil
		case setupHelpKey:
			return gogAuthSetupHelpReadonly, false, nil
		case authKey:
			return "ok", false, nil
		case headlessKey:
			return "gmail_search\n", false, nil
		case "sbx mcp ls":
			// R2-03 preflight: confirm no prior gog registration.
			return "", false, nil
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	}, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == helpKey || key == authKey || key == setupHelpKey || key == headlessKey {
			t.Fatalf("gog setup probe %q must go through the bounded probe, not raw run", key)
		}
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "add" {
			return "", nil
		}
		return "", fmt.Errorf("no fake output for %q", key)
	}, RunInteractiveFn: func(name string, args ...string) error { return nil }}}
	var out bytes.Buffer
	if err := gogSetup(env, gogSetupOpts{account: acct, credentials: cred}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	joined := strings.Join(probed, "\n")
	for _, want := range []string{helpKey, setupHelpKey, authKey, headlessKey} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q to be probed through the bounded machinery, probed=%v", want, probed)
		}
	}
}

// --- R2-03: tri-state snapshot -----------------------------------------

// gogSnapEnv builds a minimal hostenv.Env driving snapshotGogRegistration
// directly: sbx is present, and lsOut/lsTimedOut/lsErr control the bounded
// `sbx mcp ls` listing probe; getFixtures drives the detailed `sbx mcp get
// gog` / `sbx mcp ls -o json` readers (registeredGogCommand) via probeRun's
// env.Run fallback.
func gogSnapEnv(lsOut string, lsTimedOut bool, lsErr error, getFixtures map[string]string, getErrs map[string]bool) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("exec: %q not found", name)
	}, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == "sbx mcp ls" {
			return lsOut, lsTimedOut, lsErr
		}
		if getErrs[key] {
			return "", false, fmt.Errorf("exit status 1")
		}
		if out, ok := getFixtures[key]; ok {
			return out, false, nil
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	}}}
}

func TestSnapshotGogRegistration_ConfirmedAbsent(t *testing.T) {
	env := gogSnapEnv("slack\n", false, nil, nil, nil)
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegAbsent {
		t.Fatalf("expected gogRegAbsent, got state=%v argv=%v", snap.state, snap.argv)
	}
}

func TestSnapshotGogRegistration_ConfirmedPresent_RestorableArgv(t *testing.T) {
	env := gogSnapEnv("google-workspace\nslack\n", false, nil, map[string]string{
		"sbx mcp get google-workspace": "name: gog\ncommand: /usr/bin/gog --account you@example.com mcp\n",
	}, nil)
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegPresent {
		t.Fatalf("expected gogRegPresent, got state=%v", snap.state)
	}
	want := []string{"/usr/bin/gog", "--account", "you@example.com", "mcp"}
	if strings.Join(snap.argv, " ") != strings.Join(want, " ") {
		t.Errorf("expected argv %v, got %v", want, snap.argv)
	}
}

// TestSnapshotGogRegistration_ListedButUnreadable_Unknown: the bounded
// listing confirms gog IS registered, but BOTH detailed readers
// (registeredGogCommand's `sbx mcp get google-workspace` and `sbx mcp ls -o json`) come up
// with a quoted/unparseable command — must be gogRegUnknown, never absent.
func TestSnapshotGogRegistration_ListedButUnreadable_Unknown(t *testing.T) {
	env := gogSnapEnv("google-workspace\n", false, nil, map[string]string{
		// A shell-quoted command line: parseGogCommandLine explicitly refuses
		// to split this (ambiguous under strings.Fields).
		"sbx mcp get google-workspace": `name: gog` + "\n" + `command: /usr/bin/op run --env-file="/x/op refs.env" -- gog mcp` + "\n",
		"sbx mcp ls -o json":           "not json at all",
	}, nil)
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegUnknown {
		t.Fatalf("expected gogRegUnknown for a listed-but-unparseable registration, got state=%v argv=%v", snap.state, snap.argv)
	}
}

// TestSnapshotGogRegistration_ListingProbeFails_Unknown: the bounded `sbx mcp
// ls` listing itself errors (transient sbx daemon hiccup) — never treated as
// absent.
func TestSnapshotGogRegistration_ListingProbeFails_Unknown(t *testing.T) {
	env := gogSnapEnv("", false, fmt.Errorf("sbx: connection refused"), nil, nil)
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegUnknown {
		t.Fatalf("expected gogRegUnknown on a listing probe error, got state=%v", snap.state)
	}
}

// TestSnapshotGogRegistration_ListingProbeTimesOut_Unknown: same, but the
// listing probe times out rather than erroring outright.
func TestSnapshotGogRegistration_ListingProbeTimesOut_Unknown(t *testing.T) {
	env := gogSnapEnv("google-workspace\n", true, nil, nil, nil) // out would say "present", but timedOut=true wins
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegUnknown {
		t.Fatalf("expected gogRegUnknown on a listing probe timeout, got state=%v", snap.state)
	}
}

// TestSnapshotGogRegistration_GetAndJSONTransientErrors_Unknown: the bounded
// listing confirms presence, but BOTH of registeredGogCommand's own readers
// (`sbx mcp get google-workspace`, `sbx mcp ls -o json`) transiently error — confirmed
// present, unreadable command -> gogRegUnknown.
func TestSnapshotGogRegistration_GetAndJSONTransientErrors_Unknown(t *testing.T) {
	env := gogSnapEnv("google-workspace\n", false, nil, nil, map[string]bool{
		"sbx mcp get google-workspace": true,
		"sbx mcp ls -o json":           true,
	})
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegUnknown {
		t.Fatalf("expected gogRegUnknown when the detailed readers both transiently error, got state=%v", snap.state)
	}
}

func TestSnapshotGogRegistration_SbxAbsent_Unknown(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }}}
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegUnknown {
		t.Fatalf("expected gogRegUnknown when sbx is absent, got state=%v", snap.state)
	}
}

// --- R2-03/R2-04: gogSetup refuses to authorize/overwrite on an unknown
// prior registration, and never loses a confirmed one ---------------------

// gogR203PreflightEnv builds a healthy-up-to-registration gogSetup
// environment (mirrors gogR108RollbackEnv's shape) so tests can isolate just
// the prior-registration snapshot behavior. lsFixture/lsErr drive the bounded
// `sbx mcp ls` presence probe; getFixture (optional) drives `sbx mcp get google-workspace`.
func gogR203PreflightEnv(t *testing.T, lsFixture string, lsErrs bool, getFixture string) (env hostenv.Env, cred string) {
	t.Helper()
	gogSetupTestCfg(t)
	cred = gogCredFile(t)
	fixtures := map[string]string{
		"gog auth --help": gogAuthHelpCurrentSetup,
		"gog --account you@example.com auth doctor --check": "ok",
		"sbx mcp ls": lsFixture,
	}
	if getFixture != "" {
		fixtures["sbx mcp get google-workspace"] = getFixture
	}
	outputErr := map[string]bool{}
	if lsErrs {
		outputErr["sbx mcp ls"] = true
	}
	ge := gogTestEnv{
		present:       map[string]bool{"gog": true, "sbx": true},
		output:        mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), fixtures),
		outputErr:     outputErr,
		statFile:      map[string]bool{cred: true},
		sbxRegisterOK: true,
	}
	return ge.env(), cred
}

// TestGogSetup_R203_UnreadablePriorRegistration_AbortsBeforeOAuth: gog IS
// listed as registered, but its command can't be parsed (quoted) — gogSetup
// must abort with an explicit "could not confirm" error, run ZERO
// interactive commands, and leave config untouched. This proves an
// unreadable prior registration is never silently clobbered.
func TestGogSetup_R203_UnreadablePriorRegistration_AbortsBeforeOAuth(t *testing.T) {
	// gog IS listed ("sbx mcp ls" -> "google-workspace\n"), but its `sbx mcp get google-workspace`
	// command is shell-quoted (parseGogCommandLine refuses to split it), and
	// there is deliberately NO `sbx mcp ls -o json` fixture either, so
	// registeredGogCommand's fallback reader also comes up empty — confirmed
	// present, unreadable command.
	env, cred := gogR203PreflightEnv(t, "google-workspace\n", false,
		`name: gog`+"\n"+`command: /usr/bin/op run --env-file="/x/op refs.env" -- gog mcp`+"\n")

	var calls [][]string
	systest.Of(env.System).RunInteractiveFn = func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}

	pre, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pre.AddMCP("slack")
	if err := pre.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err = gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the prior gog registration is listed but unreadable")
	}
	if !strings.Contains(err.Error(), "could not confirm") {
		t.Errorf("expected the tri-state 'could not confirm' wording, got %q", err)
	}
	if len(calls) != 0 {
		t.Errorf("must run ZERO interactive commands when the prior registration is unreadable, got %v", calls)
	}
	after, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config must be byte-for-byte unchanged when the prior registration is unreadable:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestGogSetup_R203_ListingProbeTransientlyUnavailable_AbortsBeforeOAuth: the
// bounded `sbx mcp ls` listing itself errors — gogSetup must treat this as
// unknown (never absent), abort before any interactive command, and leave
// config untouched.
func TestGogSetup_R203_ListingProbeTransientlyUnavailable_AbortsBeforeOAuth(t *testing.T) {
	env, cred := gogR203PreflightEnv(t, "", true, "")

	pre, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pre.AddMCP("slack")
	if err := pre.Save(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	systest.Of(env.System).RunInteractiveFn = func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		return nil
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err = gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when the sbx mcp ls listing probe transiently fails")
	}
	if !strings.Contains(err.Error(), "could not confirm") {
		t.Errorf("expected the tri-state 'could not confirm' wording, got %q", err)
	}
	if len(calls) != 0 {
		t.Errorf("must run ZERO interactive commands on a transient listing failure, got %v", calls)
	}
	after, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config must be byte-for-byte unchanged on a transient listing failure:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestGogSetup_R203_ConfirmedAbsentPrior_ProceedsNormally: gog is confirmedly
// NOT registered (the listing succeeds and doesn't mention it) — gogSetup
// must proceed normally (never confuse "absent" with "unknown").
func TestGogSetup_R203_ConfirmedAbsentPrior_ProceedsNormally(t *testing.T) {
	env, cred := gogR203PreflightEnv(t, "slack\n", false, "")
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(env, opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
}

// --- R2-04: preflight everything before OAuth, honest help text -----------

// TestGogSetup_R204_ConfigLoadFails_AbortsBeforeOAuth: a malformed
// config.toml (toml.DecodeFile error, not mere absence) must abort gogSetup
// before any interactive command runs.
func TestGogSetup_R204_ConfigLoadFails_AbortsBeforeOAuth(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.toml"
	if err := os.WriteFile(cfgPath, []byte("this is not [valid toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", cfgPath)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		statFile: map[string]bool{cred: true},
	}
	var calls [][]string
	ge.interCalls = &calls
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when config.toml fails to parse")
	}
	if !strings.Contains(err.Error(), "loading config") {
		t.Errorf("expected a config-load-specific error, got %q", err)
	}
	if len(calls) != 0 {
		t.Errorf("must run ZERO interactive commands when config fails to load, got %v", calls)
	}
	raw, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(raw) != "this is not [valid toml" {
		t.Errorf("the unparseable config file must be left untouched, got %q", string(raw))
	}
}

// TestGogSetup_R204_SbxMissing_ZeroInteractiveCalls tightens
// TestGogSetup_R108_SbxMissing_FailsAndConfigUnchanged (gog_setup_review_
// round1_test.go) with the R2-04 assertion that specific test predates:
// sbx missing must produce ZERO runInteractive calls, not merely "eventually
// fails". Before R2-04's reordering, sbx presence was checked AFTER the
// interactive OAuth route had already run — this proves that regression
// can't recur.
func TestGogSetup_R204_SbxMissing_ZeroInteractiveCalls(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true}, // NO sbx
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
		}),
		statFile:   map[string]bool{cred: true},
		interCalls: &calls,
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when sbx is not on PATH")
	}
	if len(calls) != 0 {
		t.Errorf("must run ZERO interactive commands when sbx is missing, got %v", calls)
	}
}

// TestGogSetup_R204_HelpTextMatchesPreflightBehavior: the usage text's
// numbered steps must describe the ACTUAL preflight-before-OAuth order this
// file implements: sbx/config/registration-snapshot preflight is named
// explicitly, before the authorization step.
func TestGogSetup_R204_HelpTextMatchesPreflightBehavior(t *testing.T) {
	if !strings.Contains(gworkspaceSetupUsage, "preflights EVERY remaining predictable hard requirement BEFORE any\n     authorization happens") {
		t.Error("gworkspaceSetupUsage must describe the preflight-before-authorization sequence")
	}
	if !strings.Contains(gworkspaceSetupUsage, "sbx must be installed") {
		t.Error("gworkspaceSetupUsage must name the sbx preflight")
	}
	if !strings.Contains(gworkspaceSetupUsage, "config must load cleanly") {
		t.Error("gworkspaceSetupUsage must name the config-load preflight")
	}
	if !strings.Contains(gworkspaceSetupUsage, "CONFIRMED") {
		t.Error("gworkspaceSetupUsage must name the tri-state registration-confirmation requirement")
	}
}
