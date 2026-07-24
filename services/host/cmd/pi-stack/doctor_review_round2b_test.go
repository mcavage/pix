package main

// doctor_review_round2b_test.go covers review round 2 findings R2-01 (TOCTOU)
// and R2-02 (unbounded discovery):
//
//   R2-01  the trusted-executable gate resolved symlinks at CHECK time
//          (env.evalSymlinks) and then exec'd the ORIGINAL registered path —
//          a check-then-exec race an attacker wins by swapping the symlink
//          between the two. The fix is strict exact (cleaned) path equality
//          with the resolver's answer, NO symlink blessing, and exec'ing ONLY
//          the resolver's trusted token (never the registered spelling), for
//          the outer op wrapper, the inner gog binary, and pi-stack-host.
//   R2-02  every doctor discovery subprocess (`sbx secret ls`, `sbx mcp ls`,
//          `sbx mcp get <name>`, `sbx mcp ls -o json`, `ollama list`,
//          `op account list`) must run through the BOUNDED probe seam
//          (probeRun: hard timeout + output cap), never raw env.run — a hung
//          sbx daemon or wedged ollama must classify (present-vs-probe-failed
//          wording preserved), never hang doctor. Setup shares the same
//          bounded probeOllama.

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// recordExecs wraps env.run AND env.probe so every exec'd command line is
// recorded, letting a test assert exactly which executable tokens doctor ran.
func recordExecs(env shellEnv, execs *[]string) shellEnv {
	innerRun := env.run
	env.run = func(name string, args ...string) (string, error) {
		*execs = append(*execs, strings.Join(append([]string{name}, args...), " "))
		return innerRun(name, args...)
	}
	if innerProbe := env.probe; innerProbe != nil {
		env.probe = func(name string, args ...string) (string, bool, error) {
			*execs = append(*execs, strings.Join(append([]string{name}, args...), " "))
			return innerProbe(name, args...)
		}
	}
	return env
}

// --- R2-01: no check-then-exec of an attacker-controlled path ---------------

// TestDoctor_R201b_SwapHook_AlternateHostBinaryPathNeverExecuted: a registered
// pi-stack-host argv[0] that is NOT byte-equal (cleaned) to the resolver's
// canonical answer is NEVER executed — even if, on disk, it is a symlink that
// resolves to the real binary at check time (the attacker swaps it before the
// exec; doctor must not open that window by blessing alternate symlink paths
// at all). The fake run/probe fail the test on any attempt to exec it.
func TestDoctor_R201b_SwapHook_AlternateHostBinaryPathNeverExecuted(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	// On disk this path is a symlink pointing at the REAL canonical binary at
	// check time — and at the attacker's payload one rename later. Doctor must
	// refuse it on strict path inequality alone, never resolve-and-bless it.
	alt := "/opt/pi-stack/current/pi-stack-host"
	canonical := "/usr/local/bin/pi-stack-host"
	regCmd := alt + " mcp slack"
	f := gogConfirmed(fakeEnv{
		present:    map[string]bool{"sbx": true, "op": true},
		hostBinary: canonical,
		localMCP:   []string{"slack"},
		output: map[string]string{
			"sbx secret ls":          "anthropic openai google github",
			"sbx mcp ls":             "gog\nslack\n",
			"sbx mcp get slack":      "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools": "pwned_tool\n", // proves the exploit if ever exec'd
		},
		ports: map[int]bool{11435: true},
	})
	env := fatalOnExec(t, f.env(), alt)
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil {
		t.Fatalf("expected a slack check, groups=%+v", r.groups)
	}
	if c.evidence != EvidenceUnverifiable || !strings.Contains(c.detail, "probe skipped") {
		t.Errorf("an alternate pi-stack-host path must be skipped/unverifiable, got %+v", c)
	}
}

// TestDoctor_R201b_SwapHook_AlternateGogPathNeverExecuted: same for the inner
// gog binary of a registered gog spawn — an absolute path that is not exactly
// lookPath("gog")'s answer is never exec'd, symlink or not.
func TestDoctor_R201b_SwapHook_AlternateGogPathNeverExecuted(t *testing.T) {
	alt := "/opt/alt/gog" // symlinked to the real /usr/bin/gog at check time
	f := gogRegFixture(alt + " --account " + gogAcct + " mcp")
	env := fatalOnExec(t, f.env(), alt)
	r := runDoctor(defaultCfg(), env)
	c := findCheck(r, "gog", "headless spawn")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("an alternate gog path must be unverifiable (never exec'd), got %+v", c)
	}
	if !strings.Contains(c.detail, "does not match") {
		t.Errorf("detail should name the canonical-path mismatch, got %q", c.detail)
	}
}

// TestDoctor_R201b_SwapHook_AlternateOpPathNeverExecuted: same for the outer
// op wrapper.
func TestDoctor_R201b_SwapHook_AlternateOpPathNeverExecuted(t *testing.T) {
	alt := "/opt/alt/op" // symlinked to the real /usr/bin/op at check time
	f := gogRegFixture(alt + " run --env-file=/x/op-refs.env -- gog --account " + gogAcct + " mcp")
	env := fatalOnExec(t, f.env(), alt)
	r := runDoctor(defaultCfg(), env)
	c := findCheck(r, "gog", "headless spawn")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("an alternate op path must be unverifiable (never exec'd), got %+v", c)
	}
}

// TestDoctor_R201b_ExecsResolverTokenOnly_HostBinary: when a registration IS
// trusted (cleaned-equal to the resolver's answer), the exec'd executable
// token must be the RESOLVER's canonical path — never the registered spelling.
// A Clean-equal traversal spelling (/usr/local/lib/../bin/pi-stack-host) can
// resolve to a different real file than its cleaned form if a component is a
// symlink, so exec'ing the raw registered token is its own swap window.
func TestDoctor_R201b_ExecsResolverTokenOnly_HostBinary(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	canonical := "/usr/local/bin/pi-stack-host"
	spelled := "/usr/local/lib/../bin/pi-stack-host" // Clean-equal, not byte-equal
	f := gogConfirmed(fakeEnv{
		present:    map[string]bool{"sbx": true, "op": true},
		hostBinary: canonical,
		localMCP:   []string{"slack"},
		output: map[string]string{
			"sbx secret ls":     "anthropic openai google github",
			"sbx mcp ls":        "gog\nslack\n",
			"sbx mcp get slack": "name: slack\ncommand: " + spelled + " mcp slack\n",
			// The ONLY tool fixture is under the canonical token: doctor goes
			// green iff it exec'd the resolver's path, not the raw spelling.
			canonical + " mcp slack --list-tools": "slack_search\n",
		},
		ports: map[int]bool{11435: true},
	})
	var execs []string
	r := runDoctor(cfg, recordExecs(f.env(), &execs))
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceHealthy {
		t.Fatalf("a cleaned-equal registration must probe green via the resolver token, got %+v", c)
	}
	for _, e := range execs {
		if strings.Contains(e, "..") {
			t.Errorf("doctor exec'd the raw registered spelling instead of the resolver token: %s", e)
		}
	}
}

// TestDoctor_R201b_ExecsResolverTokenOnly_Gog: same normalization for a gog
// registration — the exec'd gog token is lookPath's answer, never the
// registered traversal spelling.
func TestDoctor_R201b_ExecsResolverTokenOnly_Gog(t *testing.T) {
	spelled := "/usr/lib/../bin/gog" // Clean-equal to lookPath's /usr/bin/gog
	regCmd := spelled + " --account " + gogAcct + " mcp"
	f := gogRegFixture(regCmd)
	f.output["/usr/bin/gog --account "+gogAcct+" mcp --list-tools"] = "gmail_search\n"
	var execs []string
	r := runDoctor(defaultCfg(), recordExecs(f.env(), &execs))
	c := findCheck(r, "gog", "headless spawn")
	if c == nil || c.evidence != EvidenceHealthy {
		t.Fatalf("a cleaned-equal gog registration must probe green via the resolver token, got %+v", c)
	}
	for _, e := range execs {
		if strings.Contains(e, "..") {
			t.Errorf("doctor exec'd the raw registered spelling instead of the resolver token: %s", e)
		}
	}
}

// --- R2-02: all discovery is bounded -----------------------------------------

// r202probe wires env.probe to a fixture map (out or a timeout) and returns
// the env. Keys are the joined command lines.
func r202probe(env shellEnv, fixtures map[string]string, hangs map[string]bool) shellEnv {
	env.probe = func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if hangs[key] {
			return "", true, context.DeadlineExceeded
		}
		if out, ok := fixtures[key]; ok {
			return out, false, nil
		}
		return "", false, fmt.Errorf("no fake probe output for %q", key)
	}
	return env
}

// TestDoctor_R202_NoRawRunInDiscovery: with the bounded probe seam wired, a
// full doctor pass (providers + ollama + gog honest path + a pi-stack-host MCP
// server + the op sign-in probe) must NEVER fall back to raw env.run — every
// noninteractive discovery subprocess goes through probeRun.
func TestDoctor_R202_NoRawRunInDiscovery(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"gog", "slack"}
	canonical := "/usr/local/bin/pi-stack-host"
	regGog := opWrappedGog(gogOpRefs, gogAcct)
	regSlack := canonical + " mcp slack"
	fixtures := map[string]string{
		"sbx secret ls":            "anthropic openai google github",
		"sbx mcp ls":               "gog\nslack\n",
		"sbx mcp get gog":          "name: gog\ncommand: " + regGog + "\n",
		regGog + " --list-tools":   "gmail_search\ncalendar_events\n",
		"sbx mcp get slack":        "name: slack\ncommand: " + regSlack + "\n",
		regSlack + " --list-tools": "slack_search\n",
		"ollama list":              "gemma4:latest\nnomic-embed-text:latest\n",
		"op account list":          "my.1password.com " + gogAcct,
		canonical + " mcp --list":  "slack",
	}
	f := fakeEnv{
		present:    map[string]bool{"sbx": true, "ollama": true, "gog": true, "op": true},
		hostBinary: canonical,
		ports:      map[int]bool{11434: true},
	}
	env := r202probe(f.env(), fixtures, nil)
	env.run = func(name string, args ...string) (string, error) {
		t.Fatalf("raw env.run called for discovery: %s",
			strings.Join(append([]string{name}, args...), " "))
		return "", nil
	}
	r := runDoctor(cfg, env)
	if c := findCheck(r, "gog", "headless spawn"); c == nil || c.evidence != EvidenceHealthy {
		t.Errorf("gog headless spawn should be healthy through the bounded seam, got %+v", c)
	}
	if c := findCheck(r, "Other MCP servers", "slack"); c == nil || c.evidence != EvidenceHealthy {
		t.Errorf("slack should be healthy through the bounded seam, got %+v", c)
	}
}

// TestDoctor_R202_SecretLsHangClassifies: a hanging `sbx secret ls` (probe
// timeout) classifies as sbx PRESENT but probe FAILED — the R1-11 wording is
// preserved — and never as "not on PATH".
func TestDoctor_R202_SecretLsHangClassifies(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		// Raw run would happily succeed — proving classification came from the
		// bounded probe, not an unbounded fallback.
		output: map[string]string{"sbx secret ls": "anthropic openai google github"},
		ports:  map[int]bool{11435: true},
	}
	env := r202probe(f.env(), nil, map[string]bool{"sbx secret ls": true})
	r := runDoctor(defaultCfg(), env)
	if r.sbxAbsent {
		t.Error("sbx binary is present — a hung probe must not read as absent")
	}
	if !r.sbxProbeFailed {
		t.Error("a hung `sbx secret ls` must classify as sbxProbeFailed")
	}
	c := findCheck(r, "Providers", "model keys")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("model keys must be unverifiable on a hung sbx probe, got %+v", c)
	}
	if !strings.Contains(c.detail, "sbx present but `sbx secret ls` failed") {
		t.Errorf("present-but-probe-failed wording must be preserved, got %q", c.detail)
	}
}

// TestDoctor_R202_McpLsHangClassifies: a hanging `sbx mcp ls` classifies as
// the gateway-down condition (sbx present, listing failed), with the existing
// wording.
func TestDoctor_R202_McpLsHangClassifies(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	f := fakeEnv{
		present:    map[string]bool{"sbx": true},
		hostBinary: localHostBinary,
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\nslack\n", // raw run would succeed
		},
		ports: map[int]bool{11435: true},
	}
	fixtures := map[string]string{
		"sbx secret ls":                 "anthropic openai google github",
		localHostBinary + " mcp --list": "slack",
	}
	env := r202probe(f.env(), fixtures, map[string]bool{"sbx mcp ls": true})
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("a hung `sbx mcp ls` must leave MCP checks unverifiable, got %+v", c)
	}
	if c.detail != gatewayDownDetail {
		t.Errorf("gateway-down wording must be preserved, got %q", c.detail)
	}
}

// TestDoctor_R202_McpGetHangClassifies: hanging `sbx mcp get <name>` AND
// `sbx mcp ls -o json` (the registered-command readers) degrade to
// "registered (tool probe unavailable)" — bounded, classified, never a wedge.
func TestDoctor_R202_McpGetHangClassifies(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	f := fakeEnv{
		present:    map[string]bool{"sbx": true},
		hostBinary: localHostBinary,
		output: map[string]string{
			// Raw run would return a probe-able registration — classification
			// must come from the bounded seam instead.
			"sbx mcp get slack": "name: slack\ncommand: /usr/local/bin/pi-stack-host mcp slack\n",
			"sbx mcp ls -o json": `[{"name":"slack","command":"/usr/local/bin/pi-stack-host",` +
				`"args":["mcp","slack"]}]`,
		},
		ports: map[int]bool{11435: true},
	}
	fixtures := map[string]string{
		"sbx secret ls":                 "anthropic openai google github",
		"sbx mcp ls":                    "gog\nslack\n",
		localHostBinary + " mcp --list": "slack",
	}
	env := r202probe(f.env(), fixtures, map[string]bool{
		"sbx mcp get slack":  true,
		"sbx mcp get gog":    true,
		"sbx mcp ls -o json": true,
	})
	r := runDoctor(cfg, env)
	c := findCheck(r, "Other MCP servers", "slack")
	if c == nil || c.evidence != EvidenceUnverifiable {
		t.Fatalf("hung registration readers must leave slack unverifiable, got %+v", c)
	}
	if !strings.Contains(c.detail, "tool probe unavailable") {
		t.Errorf("expected the registered-without-probe wording, got %q", c.detail)
	}
}

// TestDoctor_R202_OllamaListHangClassifies: a hanging `ollama list` leaves the
// model checks UNVERIFIABLE (never a confirmed pulled/not-pulled), while the
// daemon-up dial keeps the ollama check itself healthy.
func TestDoctor_R202_OllamaListHangClassifies(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"ollama": true},
		// Raw run would list every model — the unverifiable classification must
		// come from the bounded probe's timeout.
		output: map[string]string{"ollama list": "gemma4:latest\nnomic-embed-text:latest\n"},
		ports:  map[int]bool{11434: true},
	}
	env := r202probe(f.env(), nil, map[string]bool{"ollama list": true})
	r := runDoctor(defaultCfg(), env)
	if c := findCheck(r, "Ollama", "ollama"); c == nil || c.evidence != EvidenceHealthy {
		t.Fatalf("daemon-up ollama stays healthy on a hung list, got %+v", c)
	}
	for _, label := range []string{"  watcher", "  embed"} {
		c := findCheck(r, "Ollama", label)
		if c == nil || c.evidence != EvidenceUnverifiable {
			t.Errorf("%s: a hung `ollama list` must be unverifiable, got %+v", label, c)
		}
	}
}

// TestProbeOllama_SharedBoundedSeam: probeOllama — shared by doctor's Ollama
// group AND setup's receipt — reads `ollama list` through the bounded probe
// seam, never raw env.run, so setup shares the same bound.
func TestProbeOllama_SharedBoundedSeam(t *testing.T) {
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		dial:     func(int) bool { return true },
		probe: func(name string, args ...string) (string, bool, error) {
			if name == "ollama" && len(args) == 1 && args[0] == "list" {
				return "gemma4:latest\n", false, nil
			}
			return "", false, fmt.Errorf("unexpected probe")
		},
		run: func(name string, args ...string) (string, error) {
			t.Fatalf("probeOllama used raw env.run: %s %s", name, strings.Join(args, " "))
			return "", nil
		},
	}
	p := probeOllama(env)
	if !p.installed || !p.listOK || !strings.Contains(p.listOut, "gemma4") {
		t.Fatalf("probeOllama should read the list via the bounded seam, got %+v", p)
	}
}

// TestRunWithTimeout_HangingProcessBounded: the REAL bounded runner kills a
// genuinely hanging child at the deadline — a wall-clock guarantee, not just
// a seam contract.
func TestRunWithTimeout_HangingProcessBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh/sleep")
	}
	start := time.Now()
	_, timedOut, err := runWithTimeoutD(150*time.Millisecond, "sh", "-c", "sleep 30")
	elapsed := time.Since(start)
	if !timedOut {
		t.Fatalf("a hanging child must report timedOut, got timedOut=false err=%v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("bounded runner took %s — the deadline did not hold", elapsed)
	}
}
