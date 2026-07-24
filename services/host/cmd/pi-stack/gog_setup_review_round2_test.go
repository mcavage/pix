package main

// gog_setup_review_round2_test.go covers review round 2 findings R2-03,
// R2-04, and R2-05 on `pi-stack gog setup`:
//
//   R2-03  the prior gog registration snapshot is TRI-STATE: confirmed
//          absent, confirmed present with a restorable argv, or unknown
//          (the bounded `sbx mcp ls` listing itself failed/timed out, or gog
//          is listed but its registered command can't be parsed/read).
//          Before overwriting, presence is distinguished via the bounded
//          listing FIRST, independent of whether the detailed argv parses.
//          Unknown is never treated as absent: gogSetup aborts before
//          authorizing or overwriting, and rollback only ever removes a
//          confirmed-new registration or restores a confirmed prior argv.
//   R2-04  every PREDICTABLE hard requirement (config, gog path/
//          capabilities, credentials regular file, sbx binary, the
//          registration snapshot) is preflighted BEFORE the first OAuth
//          side effect. A missing sbx, a config load failure, or an unknown
//          prior registration must produce ZERO runInteractive calls and
//          leave the persisted config byte-for-byte unchanged.
//   R2-05  the credentials path must be a TRUE regular file
//          (Mode().IsRegular()), rejecting a FIFO, socket, device, or a
//          symlink to any of those; a symlink to a regular file is allowed.
//          FIFO/device coverage lives in gog_setup_review_round2_unix_test.go
//          (build-tagged `unix`, since Mkfifo and /dev/null are POSIX-only).

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// --- R2-03: tri-state snapshot -----------------------------------------

// gogSnapEnv builds a minimal shellEnv driving snapshotGogRegistration
// directly: sbx is present, and lsOut/lsTimedOut/lsErr control the bounded
// `sbx mcp ls` listing probe; getFixtures drives the detailed `sbx mcp get
// gog` / `sbx mcp ls -o json` readers (registeredGogCommand) via probeRun's
// env.run fallback.
func gogSnapEnv(lsOut string, lsTimedOut bool, lsErr error, getFixtures map[string]string, getErrs map[string]bool) shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "sbx" {
				return "/usr/bin/sbx", nil
			}
			return "", fmt.Errorf("exec: %q not found", name)
		},
		probe: func(name string, args ...string) (string, bool, error) {
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
		},
	}
}

func TestSnapshotGogRegistration_ConfirmedAbsent(t *testing.T) {
	env := gogSnapEnv("slack\n", false, nil, nil, nil)
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegAbsent {
		t.Fatalf("expected gogRegAbsent, got state=%v argv=%v", snap.state, snap.argv)
	}
}

func TestSnapshotGogRegistration_ConfirmedPresent_RestorableArgv(t *testing.T) {
	env := gogSnapEnv("gog\nslack\n", false, nil, map[string]string{
		"sbx mcp get gog": "name: gog\ncommand: /usr/bin/gog --account you@example.com mcp\n",
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
// (registeredGogCommand's `sbx mcp get gog` and `sbx mcp ls -o json`) come up
// with a quoted/unparseable command — must be gogRegUnknown, never absent.
func TestSnapshotGogRegistration_ListedButUnreadable_Unknown(t *testing.T) {
	env := gogSnapEnv("gog\n", false, nil, map[string]string{
		// A shell-quoted command line: parseGogCommandLine explicitly refuses
		// to split this (ambiguous under strings.Fields).
		"sbx mcp get gog":    `name: gog` + "\n" + `command: /usr/bin/op run --env-file="/x/op refs.env" -- gog mcp` + "\n",
		"sbx mcp ls -o json": "not json at all",
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
	env := gogSnapEnv("gog\n", true, nil, nil, nil) // out would say "present", but timedOut=true wins
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegUnknown {
		t.Fatalf("expected gogRegUnknown on a listing probe timeout, got state=%v", snap.state)
	}
}

// TestSnapshotGogRegistration_GetAndJSONTransientErrors_Unknown: the bounded
// listing confirms presence, but BOTH of registeredGogCommand's own readers
// (`sbx mcp get gog`, `sbx mcp ls -o json`) transiently error — confirmed
// present, unreadable command -> gogRegUnknown.
func TestSnapshotGogRegistration_GetAndJSONTransientErrors_Unknown(t *testing.T) {
	env := gogSnapEnv("gog\n", false, nil, nil, map[string]bool{
		"sbx mcp get gog":    true,
		"sbx mcp ls -o json": true,
	})
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegUnknown {
		t.Fatalf("expected gogRegUnknown when the detailed readers both transiently error, got state=%v", snap.state)
	}
}

func TestSnapshotGogRegistration_SbxAbsent_Unknown(t *testing.T) {
	env := shellEnv{lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") }}
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
// `sbx mcp ls` presence probe; getFixture (optional) drives `sbx mcp get gog`.
func gogR203PreflightEnv(t *testing.T, lsFixture string, lsErrs bool, getFixture string) (env shellEnv, cred string) {
	t.Helper()
	gogSetupTestCfg(t)
	cred = gogCredFile(t)
	fixtures := map[string]string{
		"gog auth --help": gogAuthHelpCurrentSetup,
		"gog --account you@example.com auth doctor --check": "ok",
		"sbx mcp ls": lsFixture,
	}
	if getFixture != "" {
		fixtures["sbx mcp get gog"] = getFixture
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
	// gog IS listed ("sbx mcp ls" -> "gog\n"), but its `sbx mcp get gog`
	// command is shell-quoted (parseGogCommandLine refuses to split it), and
	// there is deliberately NO `sbx mcp ls -o json` fixture either, so
	// registeredGogCommand's fallback reader also comes up empty — confirmed
	// present, unreadable command.
	env, cred := gogR203PreflightEnv(t, "gog\n", false,
		`name: gog`+"\n"+`command: /usr/bin/op run --env-file="/x/op refs.env" -- gog mcp`+"\n")

	var calls [][]string
	env.runInteractive = func(name string, args ...string) error {
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
	env.runInteractive = func(name string, args ...string) error {
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
	t.Setenv("PI_STACK_CONFIG", cfgPath)
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
	if !strings.Contains(gogSetupUsage, "preflights EVERY remaining predictable hard requirement BEFORE any\n     authorization happens") {
		t.Error("gogSetupUsage must describe the preflight-before-authorization sequence")
	}
	if !strings.Contains(gogSetupUsage, "sbx must be installed") {
		t.Error("gogSetupUsage must name the sbx preflight")
	}
	if !strings.Contains(gogSetupUsage, "config must load cleanly") {
		t.Error("gogSetupUsage must name the config-load preflight")
	}
	if !strings.Contains(gogSetupUsage, "CONFIRMED") {
		t.Error("gogSetupUsage must name the tri-state registration-confirmation requirement")
	}
}
