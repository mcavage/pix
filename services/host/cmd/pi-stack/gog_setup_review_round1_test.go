package main

// gog_setup_review_round1_test.go covers the gog-setup review round 1
// findings NOT already covered by gog_setup_test.go's route/idempotency tests
// (which were updated in place to require read-only capability fixtures):
//
//   R1-02  read-only OAuth authorization is ENFORCED at grant time (every
//          route's OAuth-granting step always passes --readonly, gated on a
//          capability probe of that step's own subcommand help), never
//          merely claimed; an installed CLI that cannot guarantee it fails
//          outright rather than falling back to an older, unguarded route.
//   R1-06  when op/op-refs are unavailable, gogSetup probes the BARE
//          hardened headless command directly instead of skipping
//          verification — a clean zero-tools result still fails, and a
//          timeout/exec error is unverifiable and never reported as success.
//   R1-08  `pi-stack gog setup` requires sbx present AND a successful `sbx
//          mcp add` to succeed; a registration failure (sbx missing, or `sbx
//          mcp add` itself failing) leaves the persisted config completely
//          unchanged, and a Save() failure AFTER a successful registration
//          rolls the sbx-side registration back (restoring whatever was
//          registered before, or removing the new one) rather than leaving
//          config and the gateway to drift apart.
//   R1-12  the exact subcommand help + flags are probed for the SELECTED
//          route, not just the top-level `gog auth --help` names; a route
//          whose subcommand syntax changed (missing --credentials/--login)
//          is rejected with installed-version guidance before any auth
//          command runs.
//   R1-14  (gog half) every noninteractive gog probe gogSetup makes —
//          including the new capability-help probes and the new bare
//          headless probe — goes through the bounded probe machinery, never
//          raw env.run.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
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
func gogR106Env(t *testing.T, probe func(name string, args ...string) (string, bool, error)) (shellEnv, string) {
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
	env.probe = probe
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
	if !strings.Contains(err.Error(), "headless verification failed") {
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
			if m == "gog" {
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
// the actual `sbx mcp add gog ...` call fails — config must still be left
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
			// deliberately NO "sbx mcp add gog ..." fixture and sbxRegisterOK
			// left false, so registerServers' env.run call for it errors.
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
func gogR108RollbackEnv(t *testing.T, priorRegistered bool) (env shellEnv, cred string, addCalls, rmCalls *[][]string) {
	t.Helper()
	dir := t.TempDir()
	cred = gogCredFile(t)
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
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
		fixtures["sbx mcp get gog"] = "name: gog\ncommand: " + strings.Join(priorArgv, " ") + "\n"
	}
	ge := gogTestEnv{
		present:  map[string]bool{"gog": true, "sbx": true},
		output:   mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), fixtures),
		statFile: map[string]bool{cred: true},
	}
	e := ge.env()
	baseRun := e.run
	addCalls, rmCalls = &[][]string{}, &[][]string{}
	e.run = func(name string, args ...string) (string, error) {
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
// just-added registration (`sbx mcp rm gog`), not try to restore nothing.
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
	if len(*rmCalls) != 1 || len((*rmCalls)[0]) < 3 || (*rmCalls)[0][2] != "gog" {
		t.Fatalf("expected a rollback `sbx mcp rm gog` call, got %v", *rmCalls)
	}
}

// TestGogSetup_R108_ReRunHealthySetupRemainsIdempotent: a full, real
// registration round-trip (stateful fake sbx, mirroring
// gog_setup_doctor_agreement_test.go's fake) run TWICE for the same healthy
// account must succeed both times and leave gog registered exactly once.
func TestGogSetup_R108_ReRunHealthySetupRemainsIdempotent(t *testing.T) {
	const acct = "you@example.com"
	g, cfgPath := newGogPipelineFake(t, acct)
	t.Setenv("PI_STACK_CONFIG", cfgPath)

	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		if err := gogSetup(g.env(), gogSetupOpts{account: acct, credentials: g.cred}, strings.NewReader(""), &out, false); err != nil {
			t.Fatalf("run %d: gogSetup: %v\n--- output ---\n%s", i, err, out.String())
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range cfg.MCP {
		if m == "gog" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected gog to appear exactly once in cfg.MCP after 2 healthy runs, got %v", cfg.MCP)
	}
}

// --- R1-14 (gog half): the new probes are bounded too ---

// TestGogSetup_R114_CapabilityAndBareHeadlessProbesAreBounded proves the NEW
// probes this round introduces — the per-step capability-help probe
// (gogAuthRouteCapable) and the bare headless probe (R1-06) — go through the
// bounded probe machinery (env.probe), never raw env.run, exactly like the
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
	env := shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "gog" || name == "sbx" {
				return "/usr/bin/" + name, nil
			}
			return "", fmt.Errorf("exec: %q not found", name)
		},
		statFile: func(path string) bool { return path == cred },
		getenv:   func(string) string { return "" },
		probe: func(name string, args ...string) (string, bool, error) {
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
			}
			return "", false, fmt.Errorf("no fake probe output for %q", key)
		},
		run: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if key == helpKey || key == authKey || key == setupHelpKey || key == headlessKey {
				t.Fatalf("gog setup probe %q must go through the bounded probe, not raw run", key)
			}
			if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "add" {
				return "", nil
			}
			return "", fmt.Errorf("no fake output for %q", key)
		},
		runInteractive: func(name string, args ...string) error { return nil },
	}
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
