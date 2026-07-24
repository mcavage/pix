package main

// doctor_review_round1_test.go covers review round 1 findings R1-01, R1-03,
// R1-04, R1-05, R1-07, R1-11 and the doctor half of R1-14:
//
//   R1-01  registered gog/op executables must be CANONICAL (equal to
//          env.lookPath's answer) before doctor ever execs them as a probe;
//          a mismatched path is never executed and reads as unverifiable.
//   R1-03  requirement+evidence are AUTHORITATIVE for glyph, TODO collection,
//          headline, and JSON — unverifiable / not-configured can never show
//          ✗ or contribute a repair TODO.
//   R1-04  runtime needs at least ONE model-provider key (anthropic/openai/
//          google); a single missing provider never blocks, zero-of-three does.
//   R1-05  ollama installed with the daemon down must never render healthy.
//   R1-07  gog probes are structured: clean-empty = failed (0 tools/keyring),
//          timeout / exec error = unverifiable with accurate diagnostics.
//   R1-11  sbx-binary presence is tracked separately from `sbx secret ls`
//          success; present-but-probe-failed never claims "not on PATH".
//   R1-14  doctor's gog fallback probes (`gog auth doctor --check` and the
//          setup help/version probes) run through the bounded probe machinery.

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
)

// gogRegFixture is a doctor env where sbx exposes regCmd as gog's registered
// command (the honest path). All provider keys are set so the providers group
// stays quiet.
func gogRegFixture(regCmd string) fakeEnv {
	return fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true, "op": true},
		output: map[string]string{
			"sbx secret ls":   "anthropic openai google github",
			"sbx mcp ls":      "gog\n",
			"sbx mcp get gog": "name: gog\ncommand: " + regCmd + "\n",
		},
		ports: map[int]bool{11435: true},
	}
}

// fatalOnExec wraps env.run so any attempt to exec a binary matching one of
// the forbidden tokens (in the command name OR its args) fails the test —
// proving doctor never ran the untrusted command.
func fatalOnExec(t *testing.T, env shellEnv, forbidden ...string) shellEnv {
	t.Helper()
	inner := env.run
	env.run = func(name string, args ...string) (string, error) {
		joined := strings.Join(append([]string{name}, args...), " ")
		for _, f := range forbidden {
			if strings.Contains(joined, f) {
				t.Fatalf("doctor exec'd an untrusted registered command: %s", joined)
			}
		}
		return inner(name, args...)
	}
	return env
}

// findCheck locates a labeled check in the group whose title starts with
// prefix; nil when absent.
func findCheck(r *report, titlePrefix, label string) *check {
	for gi := range r.groups {
		if !strings.HasPrefix(r.groups[gi].title, titlePrefix) {
			continue
		}
		for ci := range r.groups[gi].checks {
			if r.groups[gi].checks[ci].label == label {
				return &r.groups[gi].checks[ci]
			}
		}
	}
	return nil
}

// --- R1-01 -----------------------------------------------------------------

// A registered gog whose binary path does NOT match env.lookPath("gog") is
// never executed: the headless-spawn check is unverifiable (⚠, no TODO), not
// a ✗, and doctor never blocks on it.
func TestDoctor_R101_MaliciousGogPathNeverExecuted(t *testing.T) {
	f := gogRegFixture("/tmp/gog --account " + gogAcct + " mcp")
	env := fatalOnExec(t, f.env(), "/tmp/gog")
	r := runDoctor(defaultCfg(), env)
	c := findCheck(r, "gog", "headless spawn")
	if c == nil {
		t.Fatalf("expected a headless spawn check, groups=%+v", r.groups)
	}
	if c.evidence != EvidenceUnverifiable {
		t.Errorf("evidence = %q, want unverifiable", c.evidence)
	}
	if c.state() != stateWarn {
		t.Errorf("state = %v, want stateWarn (never ✗)", c.state())
	}
	if c.todo != "" {
		t.Errorf("untrusted registration must carry no repair TODO, got %q", c.todo)
	}
	if !strings.Contains(c.detail, "does not match") {
		t.Errorf("detail should name the canonical-path mismatch, got %q", c.detail)
	}
	if r.blocking() {
		t.Error("an untrusted gog registration must never block doctor")
	}
}

// A registered command behind a FAKE op binary (/tmp/op) is never executed.
func TestDoctor_R101_FakeOpNeverExecuted(t *testing.T) {
	f := gogRegFixture("/tmp/op run --env-file=/x/op-refs.env -- gog --account " + gogAcct + " mcp")
	env := fatalOnExec(t, f.env(), "/tmp/op")
	r := runDoctor(defaultCfg(), env)
	c := findCheck(r, "gog", "headless spawn")
	if c == nil || c.evidence != EvidenceUnverifiable || c.todo != "" {
		t.Fatalf("fake-op registration must be unverifiable with no TODO, got %+v", c)
	}
}

// A trusted op wrapper around a MISMATCHED inner gog is never executed either:
// the inner gog path must equal env.lookPath("gog") before op is spawned.
func TestDoctor_R101_InnerGogMismatchNeverExecuted(t *testing.T) {
	f := gogRegFixture("op run --env-file=/x/op-refs.env -- /tmp/gog --account " + gogAcct + " mcp")
	env := fatalOnExec(t, f.env(), "/tmp/gog")
	r := runDoctor(defaultCfg(), env)
	c := findCheck(r, "gog", "headless spawn")
	if c == nil || c.evidence != EvidenceUnverifiable || c.todo != "" {
		t.Fatalf("inner-gog mismatch must be unverifiable with no TODO, got %+v", c)
	}
}

// The trust gate must not over-block: absolute paths that MATCH lookPath's
// answers are probed and confirm green.
func TestDoctor_R101_CanonicalAbsolutePathsStillProbed(t *testing.T) {
	regCmd := "/usr/bin/op run --env-file=/x/op-refs.env -- /usr/bin/gog --account " + gogAcct + " mcp"
	f := gogRegFixture(regCmd)
	f.output[regCmd+" --list-tools"] = "gmail_search\ncalendar_events\n"
	r := runDoctor(defaultCfg(), f.env())
	c := findCheck(r, "gog", "headless spawn")
	if c == nil || c.evidence != EvidenceHealthy || c.state() != stateOK {
		t.Fatalf("canonical absolute paths must probe to a confirmed green, got %+v", c)
	}
}

// The generalized MCP probe applies the same op trust: a fake op prefix on a
// pi-stack-host registration is never executed.
func TestDoctor_R101_MCPFakeOpNeverExecuted(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	regCmd := "/tmp/op run --env-file=/x -- /usr/local/bin/pi-stack-host mcp slack"
	f := gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "op": true},
		output: map[string]string{
			"sbx secret ls":     "anthropic openai google github",
			"sbx mcp ls":        "gog\nslack\n",
			"sbx mcp get slack": "name: slack\ncommand: " + regCmd + "\n",
		},
		ports: map[int]bool{11435: true},
	})
	env := fatalOnExec(t, f.env(), "/tmp/op")
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil {
		t.Fatalf("expected a slack check, groups=%+v", r.groups)
	}
	if c.evidence != EvidenceUnverifiable {
		t.Errorf("untrusted registered command must be unverifiable, got %+v", c)
	}
	if !strings.Contains(c.detail, "probe skipped") {
		t.Errorf("detail should say the probe was skipped, got %q", c.detail)
	}
}

// --- R1-03 -----------------------------------------------------------------

// Evidence is authoritative: glyph, TODO collection, headline, and JSON all
// derive from requirement+evidence, so an unverifiable or not-configured check
// can never render ✗ or contribute a repair TODO / outstanding count.
func TestDoctor_R103_EvidenceAuthoritative(t *testing.T) {
	r := &report{groups: []group{{title: "t", checks: []check{
		{label: "unv", todo: "fix-unv", requirement: RequirementCore, evidence: EvidenceUnverifiable},
		{label: "nc", todo: "fix-nc", requirement: RequirementUnconfiguredOptional, evidence: EvidenceNotConfigured},
		{label: "optfail", todo: "fix-opt", requirement: RequirementConfiguredOptional, evidence: EvidenceFailed},
		{label: "fine", requirement: RequirementCore, evidence: EvidenceHealthy},
	}}}}

	// TODO collection: only the verified failure contributes.
	if got := r.todos(); len(got) != 1 || got[0] != "fix-opt" {
		t.Errorf("todos() = %v, want only the failed check's fix", got)
	}
	// Glyphs derive from evidence.
	states := map[string]checkState{}
	for _, c := range r.groups[0].checks {
		states[c.label] = c.state()
	}
	if states["unv"] != stateWarn {
		t.Errorf("unverifiable renders %v, want stateWarn", states["unv"])
	}
	if states["nc"] != stateInfo {
		t.Errorf("not-configured renders %v, want stateInfo", states["nc"])
	}
	if states["optfail"] != stateTODO {
		t.Errorf("verified failure renders %v, want stateTODO", states["optfail"])
	}
	if states["fine"] != stateOK {
		t.Errorf("healthy renders %v, want stateOK", states["fine"])
	}
	// Optional failure never blocks.
	if r.blocking() {
		t.Error("a configured-optional failure must not block")
	}
	// Headline counts only verified failures.
	var buf bytes.Buffer
	r.render(&buf, true)
	out := buf.String()
	if !strings.Contains(out, "1 item outstanding") {
		t.Errorf("headline should count only the verified failure, got:\n%s", out)
	}
	if strings.Contains(out, "fix-unv") || strings.Contains(out, "fix-nc") {
		t.Errorf("unverifiable/not-configured must not surface repair TODOs, got:\n%s", out)
	}
	if strings.Contains(out, "✗ unv") || strings.Contains(out, "✗ nc") {
		t.Errorf("unverifiable/not-configured must never render ✗, got:\n%s", out)
	}
	// JSON states derive from the same evidence.
	v := r.jsonView("")
	wantStates := map[string]string{"unv": "warn", "nc": "info", "optfail": "todo", "fine": "ok"}
	for _, c := range v.Groups[0].Checks {
		if want := wantStates[c.Label]; c.State != want {
			t.Errorf("JSON state for %q = %q, want %q", c.Label, c.State, want)
		}
	}
	if v.Verdict != "outstanding" {
		t.Errorf("JSON verdict = %q, want outstanding", v.Verdict)
	}

	// A verified CORE failure flips the headline and blocking.
	r2 := &report{groups: []group{{checks: []check{
		{label: "corefail", todo: "fix", requirement: RequirementCore, evidence: EvidenceFailed},
	}}}}
	if !r2.blocking() {
		t.Error("a verified core failure must block")
	}
	var buf2 bytes.Buffer
	r2.render(&buf2, true)
	if !strings.Contains(buf2.String(), "✗ pi-stack") {
		t.Errorf("blocking headline should be ✗, got:\n%s", buf2.String())
	}
}

// In the sandbox (sbx binary absent) provider checks are unverifiable: no ✗
// glyph, no provider repair TODO, no exit-1.
func TestDoctor_R103_SandboxProvidersNeverCross(t *testing.T) {
	r := runDoctor(defaultCfg(), fakeEnv{present: map[string]bool{}}.env())
	for _, label := range []string{"anthropic", "openai", "google", "model keys"} {
		c := findCheck(r, "Providers", label)
		if c == nil {
			t.Fatalf("missing provider check %q", label)
		}
		if c.evidence != EvidenceUnverifiable {
			t.Errorf("%s: evidence = %q, want unverifiable", label, c.evidence)
		}
		if c.state() == stateTODO {
			t.Errorf("%s: unverifiable provider check must not render ✗", label)
		}
	}
	for _, todo := range r.todos() {
		if strings.Contains(todo, "sbx secret set") {
			t.Errorf("unverifiable provider checks must not emit repair TODOs, got %v", r.todos())
		}
	}
	if r.blocking() {
		t.Error("sandbox-without-sbx must never block")
	}
}

// --- R1-04 -----------------------------------------------------------------

// One model-provider key of the three is enough: nothing blocks, and the
// missing providers contribute no repair TODOs.
func TestDoctor_R104_OneKeyIsEnough(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "openai\ngithub\n",
			"sbx mcp ls":    "",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	if r.blocking() {
		t.Error("one model-provider key present must not block doctor")
	}
	agg := findCheck(r, "Providers", "model keys")
	if agg == nil || agg.evidence != EvidenceHealthy || agg.requirement != RequirementCore {
		t.Fatalf("aggregate model-keys check should be core+healthy, got %+v", agg)
	}
	for _, missing := range []string{"anthropic", "google"} {
		c := findCheck(r, "Providers", missing)
		if c == nil {
			t.Fatalf("missing provider check %q", missing)
		}
		if c.evidence == EvidenceFailed || c.state() == stateTODO {
			t.Errorf("%s: an individually-missing provider must not read as a failure, got %+v", missing, c)
		}
	}
	for _, todo := range r.todos() {
		if strings.Contains(todo, "sbx secret set -g anthropic") || strings.Contains(todo, "sbx secret set -g google") {
			t.Errorf("individually-missing providers must not emit repair TODOs, got %v", r.todos())
		}
	}
}

// Zero of three model-provider keys is a verified core failure: doctor blocks
// with exactly one aggregate repair TODO.
func TestDoctor_R104_ZeroKeysBlock(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "github\n",
			"sbx mcp ls":    "",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	if !r.blocking() {
		t.Fatal("zero model-provider keys must block doctor (verified core failure)")
	}
	agg := findCheck(r, "Providers", "model keys")
	if agg == nil || agg.evidence != EvidenceFailed || agg.todo == "" {
		t.Fatalf("aggregate model-keys check should be a verified failure with a fix, got %+v", agg)
	}
	n := 0
	for _, todo := range r.todos() {
		if strings.Contains(todo, "sbx secret set -g") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected exactly ONE key repair TODO (the aggregate), got %d: %v", n, r.todos())
	}
}

// --- R1-05 -----------------------------------------------------------------

// Ollama installed but its daemon down must never render healthy: the check is
// a verified failure whose action starts the daemon, and the model checks stay
// unverifiable rather than claiming pulled state.
func TestDoctor_R105_OllamaDaemonDownNeverHealthy(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"ollama": true},
		output:  map[string]string{}, // `ollama list` errors: daemon down
		ports:   map[int]bool{},      // :11434 down
	}
	r := runDoctor(defaultCfg(), f.env())
	c := findCheck(r, "Ollama", "ollama")
	if c == nil {
		t.Fatal("expected an ollama check")
	}
	if c.evidence != EvidenceFailed || c.state() != stateTODO {
		t.Errorf("daemon-down ollama must be a verified failure (✗), got evidence=%q state=%v", c.evidence, c.state())
	}
	if !strings.Contains(c.todo, "ollama") || !strings.Contains(strings.ToLower(c.todo+c.detail), "daemon") {
		t.Errorf("action should start/check the daemon, got todo=%q detail=%q", c.todo, c.detail)
	}
	var buf bytes.Buffer
	r.render(&buf, true)
	if strings.Contains(buf.String(), "✓ ollama") {
		t.Errorf("daemon-down ollama must not render ✓, got:\n%s", buf.String())
	}
	v := r.jsonView("")
	for _, g := range v.Groups {
		for _, cj := range g.Checks {
			if cj.Label == "ollama" && cj.State == "ok" {
				t.Errorf("daemon-down ollama must not be JSON state ok: %+v", cj)
			}
		}
	}
	// Model checks must not claim a confirmed pulled/not-pulled state.
	for _, label := range []string{"  watcher", "  embed"} {
		mc := findCheck(r, "Ollama", label)
		if mc == nil || mc.evidence != EvidenceUnverifiable {
			t.Errorf("%s: want unverifiable while the daemon is down, got %+v", label, mc)
		}
	}
	if r.blocking() {
		t.Error("ollama is optional — a down daemon must never block")
	}
}

// `ollama list` succeeding proves the daemon answered even if the port dial
// was blocked: that must stay healthy.
func TestDoctor_R105_ListSuccessCountsAsDaemonUp(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"ollama": true},
		output:  map[string]string{"ollama list": "gemma4:latest\nnomic-embed-text:latest\n"},
		ports:   map[int]bool{}, // dial blocked, but list works
	}
	r := runDoctor(defaultCfg(), f.env())
	c := findCheck(r, "Ollama", "ollama")
	if c == nil || c.evidence != EvidenceHealthy {
		t.Fatalf("list-success must count as daemon up, got %+v", c)
	}
}

// --- R1-07 -----------------------------------------------------------------

// A clean empty tool list is a VERIFIED failure (keyring/creds); a timeout or
// exec error is UNVERIFIABLE with an accurate diagnostic — never mislabeled as
// a keyring failure.
func TestDoctor_R107_GogProbeOutcomes(t *testing.T) {
	regCmd := opWrappedGog("/x/op-refs.env", gogAcct)
	probeKey := regCmd + " --list-tools"

	t.Run("clean empty = failed 0 tools", func(t *testing.T) {
		f := gogRegFixture(regCmd)
		f.output[probeKey] = ""
		r := runDoctor(defaultCfg(), f.env())
		c := findCheck(r, "gog", "headless spawn")
		if c == nil || c.evidence != EvidenceFailed || !strings.Contains(c.detail, "0 tools") {
			t.Fatalf("clean-empty must be a verified 0-tools failure, got %+v", c)
		}
		if !strings.Contains(c.todo, "GOG_KEYRING_BACKEND=file") {
			t.Errorf("expected the keyring fix TODO, got %q", c.todo)
		}
	})

	t.Run("timeout = unverifiable", func(t *testing.T) {
		f := gogRegFixture(regCmd)
		env := f.env()
		env.probe = func(name string, args ...string) (string, bool, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if key == probeKey {
				return "", true, context.DeadlineExceeded
			}
			if out, ok := f.output[key]; ok {
				return out, false, nil
			}
			return "", false, fmt.Errorf("no fake probe output for %q", key)
		}
		r := runDoctor(defaultCfg(), env)
		c := findCheck(r, "gog", "headless spawn")
		if c == nil || c.evidence != EvidenceUnverifiable {
			t.Fatalf("a timed-out probe must be unverifiable, got %+v", c)
		}
		if !strings.Contains(c.detail, "timed out") {
			t.Errorf("diagnostic should say the probe timed out, got %q", c.detail)
		}
		if strings.Contains(c.detail, "keyring") || c.todo != "" {
			t.Errorf("a timeout must not be labeled a keyring failure, got %+v", c)
		}
	})

	t.Run("exec error = unverifiable", func(t *testing.T) {
		f := gogRegFixture(regCmd) // no probeKey fixture: exec errors
		r := runDoctor(defaultCfg(), f.env())
		c := findCheck(r, "gog", "headless spawn")
		if c == nil || c.evidence != EvidenceUnverifiable {
			t.Fatalf("an exec error must be unverifiable, got %+v", c)
		}
		if strings.Contains(c.detail, "keyring") || strings.Contains(c.detail, "0 tools") {
			t.Errorf("an exec error must not claim a keyring/0-tools failure, got %q", c.detail)
		}
		if c.todo != "" {
			t.Errorf("unverifiable must carry no repair TODO, got %q", c.todo)
		}
	})
}

// The generalized MCP probe distinguishes the same outcomes.
func TestDoctor_R107_MCPProbeExecErrorUnverifiable(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	regCmd := "/usr/local/bin/pi-stack-host mcp slack"
	f := gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "op": true},
		output: map[string]string{
			"sbx secret ls":     "anthropic openai google github",
			"sbx mcp ls":        "gog\nslack\n",
			"sbx mcp get slack": "name: slack\ncommand: " + regCmd + "\n",
			// no probe fixture: exec errors
		},
		ports: map[int]bool{11435: true},
	})
	r := runDoctor(cfg, f.env())
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("a failed slack probe must be unverifiable, got %+v", c)
	}
	if strings.Contains(c.detail, "0 tools") {
		t.Errorf("an exec error must not be labeled a 0-tools/keyring failure, got %q", c.detail)
	}
}

// --- R1-11 -----------------------------------------------------------------

// sbx present but `sbx secret ls` failing must say the host probe/gateway is
// unavailable — never "not on PATH" / "inside the sandbox".
func TestDoctor_R111_SbxPresentProbeFailedWording(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output:  map[string]string{}, // secret ls errors
		ports:   map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	if r.sbxAbsent {
		t.Error("sbx binary is present — sbxAbsent must be false")
	}
	if !r.sbxProbeFailed {
		t.Error("expected sbxProbeFailed when the binary exists but the probe fails")
	}
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, nil
	r.render(&buf, true)
	out := buf.String()
	if strings.Contains(out, "not on PATH") || strings.Contains(out, "inside the sandbox") {
		t.Errorf("present-but-probe-failed must not claim sbx is absent, got:\n%s", out)
	}
	if !strings.Contains(out, "sbx is on PATH") {
		t.Errorf("expected the probe/gateway-unavailable note, got:\n%s", out)
	}
	c := findCheck(r, "Providers", "model keys")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("provider checks must be unverifiable when the sbx probe fails, got %+v", c)
	}
	if !strings.Contains(c.detail, "probe") && !strings.Contains(c.detail, "gateway") {
		t.Errorf("detail should blame the sbx probe/gateway, got %q", c.detail)
	}
}

// --- R1-14 (doctor half) -----------------------------------------------------

// Doctor's fallback `gog auth doctor --check` runs through the BOUNDED probe
// machinery (env.probe), never a raw unbounded env.run.
func TestDoctor_R114_GogAuthCheckBounded(t *testing.T) {
	authKey := "gog --account " + gogAcct + " auth doctor --check"
	headlessKey := "op run --env-file=" + gogOpRefs + " -- gog --account " + gogAcct + " mcp --list-tools"
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true, "op": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\n",
		},
		envVars:  map[string]string{"GOG_ACCOUNT": gogAcct, "PI_STACK_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		ports:    map[int]bool{11435: true},
	}
	env := f.env()
	var probed []string
	env.probe = func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		probed = append(probed, key)
		switch key {
		case authKey:
			return "ok", false, nil
		case headlessKey:
			return "gmail_search\n", false, nil
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	}
	inner := env.run
	env.run = func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == authKey {
			t.Fatalf("gog auth doctor --check must go through the bounded probe, not raw run")
		}
		return inner(name, args...)
	}
	r := runDoctor(defaultCfg(), env)
	var sawAuth bool
	for _, p := range probed {
		if p == authKey {
			sawAuth = true
		}
	}
	if !sawAuth {
		t.Fatalf("expected the bounded probe to run the auth check, probed=%v", probed)
	}
	if r.blocking() {
		t.Error("nothing here should block")
	}
}

// A timed-out fallback auth check is unverifiable, not "not authorized".
func TestDoctor_R114_GogAuthCheckTimeoutUnverifiable(t *testing.T) {
	authKey := "gog --account " + gogAcct + " auth doctor --check"
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true, "op": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\n",
		},
		envVars:  map[string]string{"GOG_ACCOUNT": gogAcct, "PI_STACK_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		ports:    map[int]bool{11435: true},
	}
	env := f.env()
	env.probe = func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == authKey {
			return "", true, context.DeadlineExceeded
		}
		if out, ok := f.output[key]; ok {
			return out, false, nil
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	}
	r := runDoctor(defaultCfg(), env)
	c := findCheck(r, "gog", "account")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("a timed-out auth check must be unverifiable, got %+v", c)
	}
	if strings.Contains(c.detail, "not authorized") {
		t.Errorf("a timeout must not claim 'not authorized', got %q", c.detail)
	}
}

// gogSetup's noninteractive probes (`gog auth --help`, `gog auth doctor
// --check`) run through the bounded probe machinery too.
func TestGogSetup_R114_ProbesBounded(t *testing.T) {
	const acct = "you@example.com"
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", dir+"/config.toml")
	cred := dir + "/client.json"
	helpKey := "gog auth --help"
	authKey := "gog --account " + acct + " auth doctor --check"

	var probed []string
	env := shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "gog" {
				return "/usr/bin/gog", nil
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
			case authKey:
				return "ok", false, nil
			}
			return "", false, fmt.Errorf("no fake probe output for %q", key)
		},
		run: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if key == helpKey || key == authKey {
				t.Fatalf("gog setup probe %q must go through the bounded probe, not raw run", key)
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
	if !strings.Contains(joined, helpKey) || !strings.Contains(joined, authKey) {
		t.Fatalf("expected help+auth probes to be bounded, probed=%v", probed)
	}
}
