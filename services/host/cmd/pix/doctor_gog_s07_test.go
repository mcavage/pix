package main

// doctor_gog_s07_test.go covers the S07 gog-group semantics on the S04
// readiness axes: canonical exact-command trust (never exec an
// attacker-controlled registered path or a symlink-swapped spelling), the
// hardened read-only flags verified as evidence, non-empty tools = ready, a
// clean zero-tool list = verified todo, timeout/transport = unverifiable,
// an explicit policy denial = denied, and gog-missing / account-absent =
// optional-not-configured notes.

import (
	"fmt"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// gogCheckByLabel returns the first check with the given label from the gog
// group of a report built over env.
func gogCheckByLabel(t *testing.T, r *report, label string) (check, bool) {
	t.Helper()
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "Google Workspace") {
			continue
		}
		for _, c := range g.checks {
			if c.label == label {
				return c, true
			}
		}
	}
	return check{}, false
}

// TestDoctorGog_ReadOnlyFlagsAsEvidence: the honest path verifies the
// registered argv carries the hardened read-only flags and reports them as
// concrete evidence on a ready check.
func TestDoctorGog_ReadOnlyFlagsAsEvidence(t *testing.T) {
	f := gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	ro, ok := gogCheckByLabel(t, r, "read-only")
	if !ok {
		t.Fatalf("expected a read-only check in the gog group, groups=%+v", r.groups)
	}
	if ro.result() != verdictReady {
		t.Errorf("hardened registered command must verify ready, got %+v", ro)
	}
	for _, flag := range []string{"--readonly", "--gmail-no-send", "--wrap-untrusted"} {
		if !strings.Contains(ro.evidenceString(), flag) {
			t.Errorf("read-only evidence must cite %s, got %q", flag, ro.evidenceString())
		}
	}
	spawn, _ := gogCheckByLabel(t, r, "headless spawn")
	if spawn.result() != verdictReady {
		t.Errorf("non-empty tool list must be ready, got %+v", spawn)
	}
}

// TestDoctorGog_MissingReadOnlyFlagsIsTodo: a registered gog command WITHOUT
// the hardened read-only flags is a VERIFIED gap whose fix is the guided
// re-registration — never a raw `gog auth login` recipe.
func TestDoctorGog_MissingReadOnlyFlagsIsTodo(t *testing.T) {
	unhardened := "gog --account " + gogAcct + " mcp --allow-tool read"
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls":                "anthropic openai google github",
			"sbx mcp ls":                   "google-workspace\n",
			"sbx mcp get google-workspace": "name: gog\ncommand: " + unhardened + "\n",
			unhardened + " --list-tools":   "gmail_search\n",
		},
		ports: map[int]bool{11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	ro, ok := gogCheckByLabel(t, r, "read-only")
	if !ok {
		t.Fatalf("expected a read-only check, groups=%+v", r.groups)
	}
	if ro.result() != verdictTodo {
		t.Fatalf("missing hardened flags must be a verified todo, got %+v", ro)
	}
	if !strings.Contains(ro.todo, "pix gworkspace setup") {
		t.Errorf("the fix must be the guided setup, got %q", ro.todo)
	}
	if strings.Contains(ro.todo, "gog auth login") {
		t.Errorf("raw legacy auth guidance is banned, got %q", ro.todo)
	}
	for _, flag := range []string{"--gmail-no-send", "--wrap-untrusted", "--readonly"} {
		if !strings.Contains(ro.evidenceString(), flag) {
			t.Errorf("evidence must name the missing flag %s, got %q", flag, ro.evidenceString())
		}
	}
}

// TestDoctorGog_NonCanonicalRegisteredPathNeverExecuted: a registered command
// whose gog executable is a look-alike path (NOT the PATH-resolved canonical
// binary) must be skipped as unverifiable and NEVER executed — the classic
// attacker-controlled-registration / symlink-swap trap.
func TestDoctorGog_NonCanonicalRegisteredPathNeverExecuted(t *testing.T) {
	evil := "/tmp/gog --account " + gogAcct +
		" --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read"
	var executed []string
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true, "op": true},
		output: map[string]string{
			"sbx secret ls":                "anthropic openai google github",
			"sbx mcp ls":                   "google-workspace\n",
			"sbx mcp get google-workspace": "name: gog\ncommand: " + evil + "\n",
			// If doctor ever executed the registered spelling, this fixture
			// would answer and the probe would "succeed".
			evil + " --list-tools": "gmail_search\n",
		},
		envVars: map[string]string{"GOG_ACCOUNT": gogAcct},
		ports:   map[int]bool{11435: true},
	}
	env := f.env()
	baseRun := fakeOf(env).RunFn
	fakeOf(env).RunFn = func(name string, args ...string) (string, error) {
		if strings.HasPrefix(name, "/tmp/") {
			executed = append(executed, name)
		}
		return baseRun(name, args...)
	}
	r := runDoctor(defaultCfg(), env)
	spawn, ok := gogCheckByLabel(t, r, "headless spawn")
	if !ok {
		t.Fatalf("expected a headless spawn check, groups=%+v", r.groups)
	}
	if spawn.result() != verdictUnverifiable {
		t.Errorf("a non-canonical registered path must be unverifiable, got %+v", spawn)
	}
	if !strings.Contains(spawn.detail, "never executed") {
		t.Errorf("the detail must state the probe was never executed, got %q", spawn.detail)
	}
	if spawn.todo != "" {
		t.Errorf("an unverifiable trust-skip must carry no repair todo, got %q", spawn.todo)
	}
	if len(executed) != 0 {
		t.Fatalf("the registered look-alike path must NEVER be exec'd, got %v", executed)
	}
}

// TestDoctorGog_ZeroToolsCleanExitIsTodo: the registered command exits
// cleanly with an EMPTY tool list — a verified keyring/headless-creds todo.
func TestDoctorGog_ZeroToolsCleanExitIsTodo(t *testing.T) {
	f := gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11435: true},
	})
	// Override the probe result: clean exit, zero tools.
	f.output[opWrappedGog(gogOpRefs, gogAcct)+" --list-tools"] = "   \n"
	r := runDoctor(defaultCfg(), f.env())
	spawn, _ := gogCheckByLabel(t, r, "headless spawn")
	if spawn.result() != verdictTodo {
		t.Fatalf("a clean zero-tool list must be a verified todo, got %+v", spawn)
	}
	if !strings.Contains(spawn.todo, "GOG_KEYRING_BACKEND=file") {
		t.Errorf("expected the keyring fix, got %q", spawn.todo)
	}
}

// registeredGogProbeEnv builds a shellEnv where the registered gog command is
// read from sbx and its bounded probe is driven by probeFn (so tests can
// simulate timeouts and policy denials, which the fakeEnv map cannot).
func registeredGogProbeEnv(probeFn func() (string, bool, error)) shellEnv {
	regCmd := opWrappedGog(gogOpRefs, gogAcct)
	fixtures := map[string]string{
		"sbx secret ls":                "anthropic openai google github",
		"sbx mcp ls":                   "google-workspace\n",
		"sbx mcp get google-workspace": "name: gog\ncommand: " + regCmd + "\n",
	}
	return shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		switch name {
		case "sbx", "gog", "op":
			return "/usr/bin/" + name, nil
		}
		return "", fmt.Errorf("exec: %q not found", name)
	}, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if out, ok := fixtures[key]; ok {
			return out, nil
		}
		return "", fmt.Errorf("no fake output for %q", key)
	}, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if strings.HasSuffix(key, "--list-tools") {
			return probeFn()
		}
		if out, ok := fixtures[key]; ok {
			return out, false, nil
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	}, GetenvFn: func(k string) string {
		if k == "PIX_CONFIG" {
			return gogCfgFile
		}
		return ""
	}, IsFileFn: func(p string) bool { return p == gogOpRefs }, DialLocalFn: func(int) bool { return false }}}
}

// TestDoctorGog_ProbeTimeoutIsUnverifiable: a timed-out probe of the
// registered command is UNVERIFIABLE — never the keyring todo, never a ✗.
func TestDoctorGog_ProbeTimeoutIsUnverifiable(t *testing.T) {
	env := registeredGogProbeEnv(func() (string, bool, error) {
		return "", true, fmt.Errorf("context deadline exceeded")
	})
	r := runDoctor(defaultCfg(), env)
	spawn, ok := gogCheckByLabel(t, r, "headless spawn")
	if !ok {
		t.Fatalf("expected a headless spawn check, groups=%+v", r.groups)
	}
	if spawn.result() != verdictUnverifiable {
		t.Errorf("a probe timeout must be unverifiable, got %+v", spawn)
	}
	if spawn.todo != "" {
		t.Errorf("an unverifiable probe must not surface a repair todo, got %q", spawn.todo)
	}
	if !strings.Contains(spawn.detail, "timed out") {
		t.Errorf("the detail must say it timed out, got %q", spawn.detail)
	}
}

// TestDoctorGog_ExplicitPolicyDenialIsDenied: the registered command fails
// with an EXPLICIT policy denial in its output — verdict denied (a positive
// organizational refusal, not a setup todo), and since gog is optional it
// still never blocks doctor's exit code.
func TestDoctorGog_ExplicitPolicyDenialIsDenied(t *testing.T) {
	env := registeredGogProbeEnv(func() (string, bool, error) {
		return "request rejected: access denied by your organization's policy",
			false, fmt.Errorf("exit status 1")
	})
	r := runDoctor(defaultCfg(), env)
	spawn, ok := gogCheckByLabel(t, r, "headless spawn")
	if !ok {
		t.Fatalf("expected a headless spawn check, groups=%+v", r.groups)
	}
	if spawn.result() != verdictDenied {
		t.Fatalf("an explicit policy denial must classify as denied, got %+v", spawn)
	}
	if r.blocking() {
		t.Error("gog is optional — a denied gog check must never block doctor's exit code")
	}
}

// TestDoctorGog_GenericProbeErrorIsUnverifiable: a non-zero exit WITHOUT any
// denial token stays unverifiable (doctor does not know), never denied and
// never the keyring todo.
func TestDoctorGog_GenericProbeErrorIsUnverifiable(t *testing.T) {
	env := registeredGogProbeEnv(func() (string, bool, error) {
		return "connection refused", false, fmt.Errorf("exit status 1")
	})
	r := runDoctor(defaultCfg(), env)
	spawn, _ := gogCheckByLabel(t, r, "headless spawn")
	if spawn.result() != verdictUnverifiable {
		t.Errorf("a generic probe failure must be unverifiable, got %+v", spawn)
	}
}

// TestDoctorGog_MissingCLIIsNotConfiguredNote: no gog anywhere — an expected
// absence: a note pointing at the guided setup, never a failure/TODO.
func TestDoctorGog_MissingCLIIsNotConfiguredNote(t *testing.T) {
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}, ports: map[int]bool{}}
	r := runDoctor(defaultCfg(), f.env())
	cli, ok := gogCheckByLabel(t, r, "dependency CLI")
	if !ok {
		t.Fatalf("expected a dependency CLI line, groups=%+v", r.groups)
	}
	if !cli.note {
		t.Errorf("a missing gog CLI is optional-not-configured (a note), got %+v", cli)
	}
	if !strings.Contains(cli.detail, "pix gworkspace setup") {
		t.Errorf("the note must point at the guided setup, got %q", cli.detail)
	}
	joined := strings.Join(r.todos(), "\n")
	if strings.Contains(joined, "brew install gog") {
		t.Errorf("a missing optional gog must not surface an install TODO, got %v", r.todos())
	}
}
