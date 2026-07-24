package main

// gog_setup_review_round3_test.go covers ship-review finding #1 (round 3):
// gogSetup previously resolved the gog registrar TWICE — once (implicitly,
// via gogHeadlessProbe -> gogRegisteredArgv) to probe the headless path, and
// again, independently, inside registerServers's op/op-refs/gog lookups when
// it actually registered with the sbx gateway. Another process mutating
// PATH (a different `gog` binary) or op-refs.env's resolved path between
// those two points could make the REGISTERED command differ from the one
// that was just proven healthy — a TOCTOU / config-registration-drift bug.
//
// The fix (buildGogRegistrar in mcp.go) resolves op/op-refs/gog into ONE
// immutable mcpRegistrar snapshot, exactly once, before the headless probe;
// the same snapshot is reused unchanged for registration. These tests prove
// that invariant directly: mutate PATH/op-refs AFTER the probe fires, and
// assert the argv actually registered is byte-for-byte identical to the argv
// that was probed — never the post-mutation value.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// gogTOCTOUEnv builds a hermetic shellEnv for the mutation-hook tests below.
// currentGog is a MUTABLE pointer the lookPath closure reads on every call
// (so a hook firing mid-run can change what a LATER, buggy re-resolution
// would see); currentOpRefs, when non-nil, makes "op" resolve on PATH.
// mutateOnProbe fires exactly once, the instant the headless `--list-tools`
// probe call is served, simulating a race that lands in the window between
// probe and registration. statFile/fileMode go straight to the real
// filesystem (os.Stat), so every path used in these tests is a real temp
// file/dir rather than a hand-maintained fixture map.
type gogTOCTOUEnv struct {
	currentGog    *string
	currentOpRefs *string // non-nil => "op" is on PATH; nil => op absent
	account       string
	mutateOnProbe func()
	probedArgv    *[]string // set to the argv the headless probe actually ran
	addArgs       *[][]string
	rmArgs        *[][]string
}

func (g gogTOCTOUEnv) env() shellEnv {
	fixed := mergeOutputs(gogAuthCapabilityFixtures("setup", true), map[string]string{
		"gog auth --help": gogAuthHelpCurrentSetup,
		"gog --account " + g.account + " auth doctor --check": "ok",
	})
	mutated := false
	return shellEnv{
		lookPath: func(name string) (string, error) {
			switch name {
			case "gog":
				return *g.currentGog, nil
			case "sbx":
				return "/usr/bin/sbx", nil
			case "op":
				if g.currentOpRefs != nil {
					return "/usr/bin/op", nil
				}
				return "", fmt.Errorf("not found")
			default:
				return "", fmt.Errorf("exec: %q not found", name)
			}
		},
		run: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if strings.HasSuffix(key, "--list-tools") {
				// This IS the headless verification probe. Record the exact argv it
				// ran, then fire the mutation hook exactly once — simulating PATH or
				// op-refs.env changing in the window right after the probe returns
				// and right before registration would run.
				*g.probedArgv = append([]string{name}, args...)
				if !mutated && g.mutateOnProbe != nil {
					mutated = true
					g.mutateOnProbe()
				}
				return "gmail_search\ncalendar_events\n", nil
			}
			if name == "sbx" && len(args) >= 2 && args[0] == "mcp" {
				switch args[1] {
				case "add":
					*g.addArgs = append(*g.addArgs, append([]string{}, args...))
					return "", nil
				case "rm":
					*g.rmArgs = append(*g.rmArgs, append([]string{}, args...))
					return "", nil
				case "ls":
					return "", nil // confirmed absent prior registration
				}
			}
			if out, ok := fixed[key]; ok {
				return out, nil
			}
			return "", fmt.Errorf("no fake output for %q", key)
		},
		getenv: os.Getenv,
		statFile: func(path string) bool {
			info, err := os.Stat(path)
			return err == nil && !info.IsDir()
		},
		fileMode: func(path string) (os.FileMode, bool) {
			info, err := os.Stat(path)
			if err != nil {
				return 0, false
			}
			return info.Mode(), true
		},
		runInteractive: func(name string, args ...string) error { return nil },
	}
}

// TestGogSetup_R3_PathMutatesAfterProbe_RegisteredArgvMatchesProbedArgv is the
// PATH-mutation half of finding #1: gog resolves to one absolute path for the
// probe, then a DIFFERENT binary appears on PATH before registration would
// run (an attacker swap, or an unrelated process editing PATH). The
// registered add argv must derive from the SAME gog path that was probed —
// never the post-probe one.
func TestGogSetup_R3_PathMutatesAfterProbe_RegisteredArgvMatchesProbedArgv(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	const goodGog = "/usr/bin/gog"
	const evilGog = "/opt/evil/gog"
	gogPath := goodGog

	var probedArgv []string
	var addArgs, rmArgs [][]string
	ge := gogTOCTOUEnv{
		currentGog: &gogPath,
		account:    "you@example.com",
		mutateOnProbe: func() {
			gogPath = evilGog // PATH mutated: `gog` now resolves elsewhere
		},
		probedArgv: &probedArgv,
		addArgs:    &addArgs,
		rmArgs:     &rmArgs,
	}
	env := ge.env()

	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(env, opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}

	if len(probedArgv) == 0 {
		t.Fatal("expected the headless probe to run")
	}
	if len(addArgs) != 1 {
		t.Fatalf("expected exactly 1 `sbx mcp add` call, got %d: %v", len(addArgs), addArgs)
	}
	if gogPath != evilGog {
		t.Fatal("sanity: the mutation hook did not fire")
	}

	registered := addArgs[0]
	for _, a := range registered {
		if a == evilGog {
			t.Fatalf("registered add argv used the POST-PROBE mutated PATH (%s): re-resolved after the probe instead of "+
				"reusing the snapshot, got %v", evilGog, registered)
		}
	}
	// byte-for-byte: rawAddArgs of the probed argv (minus probeListTools' own
	// trailing "--list-tools") must equal exactly what was registered.
	wantExec := probedArgv[:len(probedArgv)-1]
	want := rawAddArgs("gog", wantExec)
	if strings.Join(registered, " ") != strings.Join(want, " ") {
		t.Errorf("registered add argv = %v, want exactly the probed argv (via rawAddArgs) = %v", registered, want)
	}
	if !strings.Contains(strings.Join(registered, " "), goodGog) {
		t.Errorf("registered add argv must use the PRE-mutation (probed) gog path %s, got %v", goodGog, registered)
	}
}

// TestGogSetup_R3_OpRefsMutatesAfterProbe_RegisteredArgvMatchesProbedArgv is
// the op-refs half of finding #1: resolveOpRefs derives its answer from
// $PI_STACK_CONFIG's directory, so mutating THAT env var mid-run changes
// which op-refs.env a re-resolution would find — exactly the race a
// TOCTOU-vulnerable re-resolve would expose. op-refs.env resolves to one path
// for the probe (wrapping gog in `op run --env-file=<path>`), then
// $PI_STACK_CONFIG is mutated to point at a DIFFERENT op-refs.env before
// registration would run. The registered add argv's --env-file must match
// exactly what was probed, never the post-mutation path.
func TestGogSetup_R3_OpRefsMutatesAfterProbe_RegisteredArgvMatchesProbedArgv(t *testing.T) {
	goodDir := t.TempDir()
	evilDir := t.TempDir()
	goodRefs := filepath.Join(goodDir, "op-refs.env")
	evilRefs := filepath.Join(evilDir, "op-refs.env")
	if err := os.WriteFile(goodRefs, []byte("GOG_ACCOUNT=you@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evilRefs, []byte("GOG_ACCOUNT=attacker@evil.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goodConfig := filepath.Join(goodDir, "config.toml")
	evilConfig := filepath.Join(evilDir, "config.toml")
	t.Setenv("PI_STACK_CONFIG", goodConfig)

	cred := gogCredFile(t)
	gogPath := "/usr/bin/gog"
	var probedArgv []string
	var addArgs, rmArgs [][]string
	ge := gogTOCTOUEnv{
		currentGog:    &gogPath,
		currentOpRefs: &goodRefs, // non-nil => "op" resolves on PATH
		account:       "you@example.com",
		mutateOnProbe: func() {
			// $PI_STACK_CONFIG mutated: a later resolveOpRefs() call would now
			// find evilRefs instead of goodRefs.
			os.Setenv("PI_STACK_CONFIG", evilConfig)
		},
		probedArgv: &probedArgv,
		addArgs:    &addArgs,
		rmArgs:     &rmArgs,
	}
	env := ge.env()

	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(env, opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}

	if len(probedArgv) == 0 {
		t.Fatal("expected the headless probe to run")
	}
	if len(addArgs) != 1 {
		t.Fatalf("expected exactly 1 `sbx mcp add` call, got %d: %v", len(addArgs), addArgs)
	}
	if os.Getenv("PI_STACK_CONFIG") != evilConfig {
		t.Fatal("sanity: the mutation hook did not fire")
	}

	registered := addArgs[0]
	for _, a := range registered {
		if a == "--env-file="+evilRefs {
			t.Fatalf("registered add argv used the POST-PROBE mutated op-refs path (%s): re-resolved after the "+
				"probe instead of reusing the snapshot, got %v", evilRefs, registered)
		}
	}
	wantExec := probedArgv[:len(probedArgv)-1]
	want := rawAddArgs("gog", wantExec)
	if strings.Join(registered, " ") != strings.Join(want, " ") {
		t.Errorf("registered add argv = %v, want exactly the probed argv (via rawAddArgs) = %v", registered, want)
	}
	if !strings.Contains(strings.Join(registered, " "), "--env-file="+goodRefs) {
		t.Errorf("registered add argv must use the PRE-mutation (probed) op-refs path %s, got %v", goodRefs, registered)
	}
}

// TestGogSetup_R3_RegistrationFailureAfterMutation_ConfigNeverSaved proves the
// other half of the invariant: config.Save() only ever runs AFTER a
// successful registration. Here the sbx `mcp add` call is made to fail (no
// fixture, sbxRegisterOK left off) even after a PATH mutation between probe
// and registration — config must be left byte-for-byte unchanged either way.
func TestGogSetup_R3_RegistrationFailureAfterMutation_ConfigNeverSaved(t *testing.T) {
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

	gogPath := "/usr/bin/gog"
	var probedArgv []string
	var addArgs, rmArgs [][]string
	ge := gogTOCTOUEnv{
		currentGog: &gogPath,
		account:    "you@example.com",
		mutateOnProbe: func() {
			gogPath = "/opt/evil/gog"
		},
		probedArgv: &probedArgv,
		addArgs:    &addArgs, // never populated: env.run below fails every "sbx mcp add"
		rmArgs:     &rmArgs,
	}
	env := ge.env()
	baseRun := env.run
	env.run = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "add" {
			return "", fmt.Errorf("sbx: connection refused")
		}
		return baseRun(name, args...)
	}

	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err = gogSetup(env, opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when sbx mcp add fails")
	}
	after, err := os.ReadFile(config.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("config must be byte-for-byte unchanged when registration fails (even after a PATH mutation):\nbefore=%q\nafter=%q", before, after)
	}
}
