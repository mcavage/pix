package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pi-stack/host/config"
)

// doctor_mcp_test.go — S05 doctor MCP truth: registration states per kind,
// receipt-backed attachment evidence, native remote OAuth outcomes, fail-closed
// classification, stale/unknown config keys, bounded subprocess probes, and the
// canonical-executable / TOCTOU exec gate.

// findCheck returns the first check in g whose label matches.
func findCheck(t *testing.T, g group, label string) check {
	t.Helper()
	for _, c := range g.checks {
		if c.label == label {
			return c
		}
	}
	t.Fatalf("no check labeled %q in group %+v", label, g)
	return check{}
}

// hasCheck reports whether g carries a check with the label.
func hasCheck(g group, label string) bool {
	for _, c := range g.checks {
		if c.label == label {
			return true
		}
	}
	return false
}

// mcpFake builds a fakeEnv with the canonical pi-stack-host resolved and slack
// confirmed local — the baseline most subtests start from.
func mcpFake() fakeEnv {
	return fakeEnv{
		present: map[string]bool{"sbx": true},
		hostBin: "/usr/local/bin/pi-stack-host",
		output: map[string]string{
			"/usr/local/bin/pi-stack-host mcp --list": "slack\n",
		},
	}
}

var noCtx = mcpSandboxContext{mode: mcpAttachNone}

// TestMCPRegistrationStates covers the registration axis for every kind:
// verified from `sbx mcp ls` only, with type-correct repair guidance.
func TestMCPRegistrationStates(t *testing.T) {
	cfg := defaultCfg()

	t.Run("local not registered -> register TODO", func(t *testing.T) {
		cfg := defaultCfg()
		cfg.MCP = []string{"slack"}
		g := mcpGroupWith(cfg, mcpFake().env(), "gog\n", true, true, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictTodo || c.todo != "pi-stack mcp register slack" {
			t.Errorf("local not-registered = %+v, want todo `pi-stack mcp register slack`", c)
		}
	})

	t.Run("catalog not registered -> bundle TODO", func(t *testing.T) {
		cfg.MCP = []string{"notion"}
		g := mcpGroupWith(cfg, mcpFake().env(), "gog\n", true, true, nil, noCtx)
		c := findCheck(t, g, "notion")
		if c.result() != verdictTodo || c.todo != "pi-stack mcp bundle" {
			t.Errorf("catalog not-registered = %+v, want todo `pi-stack mcp bundle`", c)
		}
		if !strings.Contains(c.detail, "pi-stack mcp auth notion") {
			t.Errorf("catalog guidance should mention the auth step: %q", c.detail)
		}
	})

	t.Run("pack remote not registered -> register TODO", func(t *testing.T) {
		cfg.MCP = nil
		containers := map[string]packContainer{"acme": {RemoteURL: "https://mcp.acme.example/sse"}}
		g := mcpGroupWith(cfg, mcpFake().env(), "gog\n", true, true, containers, noCtx)
		c := findCheck(t, g, "acme")
		if c.result() != verdictTodo || c.todo != "pi-stack mcp register acme" {
			t.Errorf("pack-remote not-registered = %+v, want todo `pi-stack mcp register acme`", c)
		}
	})

	t.Run("custom not registered -> native sbx guidance, never bundle/register", func(t *testing.T) {
		cfg.MCP = []string{"linear"}
		g := mcpGroupWith(cfg, mcpFake().env(), "gog\n", true, true, nil, noCtx)
		c := findCheck(t, g, "linear")
		if c.result() != verdictTodo {
			t.Errorf("confirmed-missing custom server must be a verified todo, got %+v", c)
		}
		if strings.Contains(c.todo, "pi-stack mcp register") || strings.Contains(c.todo, "pi-stack mcp bundle") {
			t.Errorf("custom repair must not name a pi-stack command that can't register it: %q", c.todo)
		}
		if !strings.Contains(c.detail, "sbx mcp add") {
			t.Errorf("custom guidance should point at native sbx mcp add: %q", c.detail)
		}
	})

	t.Run("registered local, command unreadable -> unverifiable, no false green", func(t *testing.T) {
		cfg.MCP = []string{"slack"}
		g := mcpGroupWith(cfg, mcpFake().env(), "slack\n", true, true, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictUnverifiable {
			t.Errorf("registered w/o readable command = %+v, want unverifiable", c)
		}
	})

	t.Run("mcp ls failed, sbx present -> unverifiable gateway detail", func(t *testing.T) {
		cfg.MCP = []string{"slack"}
		g := mcpGroupWith(cfg, mcpFake().env(), "", false, true, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictUnverifiable || !strings.Contains(c.detail, "sbx daemon") {
			t.Errorf("gateway-down = %+v, want unverifiable + daemon detail", c)
		}
	})

	t.Run("sbx absent -> unverifiable", func(t *testing.T) {
		cfg.MCP = []string{"slack"}
		f := mcpFake()
		f.present = map[string]bool{}
		g := mcpGroupWith(cfg, f.env(), "", false, false, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictUnverifiable {
			t.Errorf("sbx-absent = %+v, want unverifiable", c)
		}
	})
}

// receiptEnv builds a fakeEnv whose getwd/stateDir seams point at a real
// t.TempDir() receipt store for workspace ws (sandbox pi-stack-<base(ws)>).
func receiptEnv(t *testing.T, f fakeEnv, ws string) (shellEnv, string) {
	t.Helper()
	stateDir := t.TempDir()
	env := f.env()
	env.getwd = func() (string, error) { return ws, nil }
	env.stateDir = func() (string, error) { return stateDir, nil }
	return env, stateDir
}

var receiptClock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

// TestMCPAttachmentFromReceipt covers the attachment axis: ONLY the launcher
// receipt (a record of successful pi-stack actions) may claim attachment.
func TestMCPAttachmentFromReceipt(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	const ws = "/home/u/proj"
	const box = "pi-stack-proj"
	// slack registered; its definition unreadable (fine — attachment is a
	// separate axis) and the sandbox present in `sbx ls`.
	regOut := "slack\n"
	base := mcpFake()
	base.output["sbx ls"] = box + "  running  " + ws + "\n"

	t.Run("preloaded at create -> ready with receipt evidence", func(t *testing.T) {
		env, stateDir := receiptEnv(t, base, ws)
		if err := writeCreateReceipt(stateDir, box, []string{"slack"}, receiptClock); err != nil {
			t.Fatal(err)
		}
		g := mcpGroupWith(cfg, env, regOut, true, true, nil, resolveMCPSandboxContext(env))
		c := findCheck(t, g, "slack attachment")
		if c.result() != verdictReady || c.evidence != "preloaded by pi-stack at create" {
			t.Errorf("preloaded attach = %+v, want ready with `preloaded by pi-stack at create`", c)
		}
	})

	t.Run("live load receipt -> ready `loaded by pi-stack`", func(t *testing.T) {
		env, stateDir := receiptEnv(t, base, ws)
		if err := writeCreateReceipt(stateDir, box, nil, receiptClock); err != nil {
			t.Fatal(err)
		}
		if err := appendLoadReceipt(stateDir, box, "slack", receiptClock); err != nil {
			t.Fatal(err)
		}
		g := mcpGroupWith(cfg, env, regOut, true, true, nil, resolveMCPSandboxContext(env))
		c := findCheck(t, g, "slack attachment")
		if c.result() != verdictReady || c.evidence != "loaded by pi-stack" {
			t.Errorf("loaded attach = %+v, want ready with `loaded by pi-stack`", c)
		}
	})

	t.Run("receipt exists but no entry for the server -> unverifiable + exact guidance", func(t *testing.T) {
		env, stateDir := receiptEnv(t, base, ws)
		if err := writeCreateReceipt(stateDir, box, []string{"notion"}, receiptClock); err != nil {
			t.Fatal(err)
		}
		g := mcpGroupWith(cfg, env, regOut, true, true, nil, resolveMCPSandboxContext(env))
		c := findCheck(t, g, "slack attachment")
		if c.result() != verdictUnverifiable {
			t.Errorf("no-entry attach must be unverifiable (config is not attachment), got %+v", c)
		}
		if !strings.Contains(c.detail, "pi-stack mcp load slack") || !strings.Contains(c.detail, "pi-stack run --replace") {
			t.Errorf("guidance must carry the exact commands: %q", c.detail)
		}
		if c.todo != "" {
			t.Errorf("unverifiable attachment must not surface a repair TODO: %q", c.todo)
		}
	})

	t.Run("no receipt at all -> unverifiable, never a claim", func(t *testing.T) {
		env, _ := receiptEnv(t, base, ws)
		g := mcpGroupWith(cfg, env, regOut, true, true, nil, resolveMCPSandboxContext(env))
		c := findCheck(t, g, "slack attachment")
		if c.result() != verdictUnverifiable || !strings.Contains(c.detail, "pi-stack mcp load slack") {
			t.Errorf("absent-receipt attach = %+v, want unverifiable + load guidance", c)
		}
	})

	for name, contents := range map[string]string{
		"corrupt":           "not json{",
		"schema-mismatch":   `{"schema":99,"sandbox":"` + box + `","preloaded":["slack"]}`,
		"identity-mismatch": `{"schema":1,"sandbox":"someone-else","preloaded":["slack"]}`,
	} {
		t.Run(name+" receipt -> unverifiable, never trusted", func(t *testing.T) {
			env, stateDir := receiptEnv(t, base, ws)
			dir := filepath.Join(stateDir, "sandboxes", box)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			g := mcpGroupWith(cfg, env, regOut, true, true, nil, resolveMCPSandboxContext(env))
			c := findCheck(t, g, "slack attachment")
			if c.result() != verdictUnverifiable {
				t.Errorf("%s receipt = %+v, want unverifiable — a bad receipt must never claim attachment", name, c)
			}
			if !strings.Contains(c.detail, name) {
				t.Errorf("detail should name the receipt state %q: %q", name, c.detail)
			}
		})
	}
}

// TestMCPAttachmentSurvivesDeregistration pins finding #1 at the doctor
// layer: a valid preload receipt still renders READY even though the server
// is now positively deregistered (`sbx mcp ls` lacks it) — registration is a
// separate, present-tense fact and never proves the sandbox was unloaded.
// The registration LINE itself still correctly flags the deregistration with
// its own type-correct repair TODO; only the attachment line is dominated by
// the receipt.
func TestMCPAttachmentSurvivesDeregistration(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	const ws = "/home/u/proj"
	const box = "pi-stack-proj"
	base := mcpFake()
	base.output["sbx ls"] = box + "  running  " + ws + "\n"
	env, stateDir := receiptEnv(t, base, ws)
	if err := writeCreateReceipt(stateDir, box, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	// slack is now DEREGISTERED (the `sbx mcp ls` output lacks it).
	g := mcpGroupWith(cfg, env, "gog\n", true, true, nil, resolveMCPSandboxContext(env))
	c := findCheck(t, g, "slack attachment")
	if c.result() != verdictReady || !strings.Contains(c.evidence, "preloaded by pi-stack at create") {
		t.Errorf("attach = %+v, want ready — the receipt dominates deregistration", c)
	}
	if !strings.Contains(c.evidence, "currently not registered") {
		t.Errorf("evidence should still name the current dereg reading: %q", c.evidence)
	}
	reg := findCheck(t, g, "slack")
	if reg.result() != verdictTodo || reg.todo != "pi-stack mcp register slack" {
		t.Errorf("registration line = %+v, want its own register TODO (deregistration is still real)", reg)
	}
}

// TestMCPAttachmentSurvivesUnknownRegistration: same precedence, the other
// axis — an UNKNOWN current registration reading (the `sbx mcp ls` listing
// itself failed) must not block a valid receipt's positive claim either.
func TestMCPAttachmentSurvivesUnknownRegistration(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	const ws = "/home/u/proj"
	const box = "pi-stack-proj"
	base := mcpFake()
	base.output["sbx ls"] = box + "  running  " + ws + "\n"
	env, stateDir := receiptEnv(t, base, ws)
	if err := writeCreateReceipt(stateDir, box, []string{"slack"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	// mcpOK=false: the registration listing itself failed — unknown, not "no".
	g := mcpGroupWith(cfg, env, "", false, true, nil, resolveMCPSandboxContext(env))
	c := findCheck(t, g, "slack attachment")
	if c.result() != verdictReady || !strings.Contains(c.evidence, "preloaded by pi-stack at create") {
		t.Errorf("attach = %+v, want ready — the receipt survives an unknown registration reading", c)
	}
	if !strings.Contains(c.evidence, "registration unknown") {
		t.Errorf("evidence should name the registration-unknown fact: %q", c.evidence)
	}
}

// TestMCPGroupIncludesReceiptOnlyName pins finding #2 at the doctor layer: a
// sandbox's own receipt names a server that is NOT part of the current
// cfg.MCP/pack-integration intent (a transient `run --pack` mix-in, or a
// since-switched pack's historical integration) — it must still surface,
// evidence LABELED as sandbox provenance rather than current preload intent.
func TestMCPGroupIncludesReceiptOnlyName(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"} // current intent: slack only
	const ws = "/home/u/proj"
	const box = "pi-stack-proj"
	base := mcpFake()
	base.output["sbx ls"] = box + "  running  " + ws + "\n"
	env, stateDir := receiptEnv(t, base, ws)
	// The receipt preloaded BOTH slack (current intent) and notion (no longer
	// configured).
	if err := writeCreateReceipt(stateDir, box, []string{"slack", "notion"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	g := mcpGroupWith(cfg, env, "slack\nnotion\n", true, true, nil, resolveMCPSandboxContext(env))
	if !hasCheck(g, "notion attachment") {
		t.Fatalf("expected notion's attachment even though it's not in cfg.MCP: %+v", g)
	}
	attach := findCheck(t, g, "notion attachment")
	if attach.result() != verdictReady {
		t.Errorf("notion attach = %+v, want ready (preloaded)", attach)
	}
	if !strings.Contains(attach.evidence, "sandbox provenance only") {
		t.Errorf("notion attach evidence should be labeled sandbox provenance: %q", attach.evidence)
	}
	reg := findCheck(t, g, "notion")
	if !strings.Contains(reg.evidence, "sandbox provenance only") {
		t.Errorf("notion registration-line evidence = %q, want the sandbox-provenance label", reg.evidence)
	}
	// slack (current intent) must NOT carry the receipt-only label.
	slackReg := findCheck(t, g, "slack")
	if strings.Contains(slackReg.evidence, "sandbox provenance only") {
		t.Errorf("slack (current intent) must not carry the receipt-only label: %q", slackReg.evidence)
	}
}

// TestMCPGroupSwitchedPackKeepsOldIntegrationVisible: the active pack has
// SWITCHED (the current containers no longer name the old integration), but
// this sandbox's own receipt still preloaded it — it must remain visible,
// labeled as sandbox provenance, alongside the NEW pack's own integration.
func TestMCPGroupSwitchedPackKeepsOldIntegrationVisible(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = nil // the old pack's integration was never persisted to cfg.MCP
	const ws = "/home/u/proj"
	const box = "pi-stack-proj"
	base := mcpFake()
	base.output["sbx ls"] = box + "  running  " + ws + "\n"
	env, stateDir := receiptEnv(t, base, ws)
	if err := writeCreateReceipt(stateDir, box, []string{"acme-remote"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	newContainers := map[string]packContainer{"newco": {RemoteURL: "https://mcp.newco.example/sse"}}
	g := mcpGroupWith(cfg, env, "newco\n", true, true, newContainers, resolveMCPSandboxContext(env))
	if !hasCheck(g, "acme-remote attachment") {
		t.Fatalf("switched-pack historical MCP provenance must remain visible: %+v", g)
	}
	c := findCheck(t, g, "acme-remote attachment")
	if c.result() != verdictReady || !strings.Contains(c.evidence, "sandbox provenance only") {
		t.Errorf("acme-remote attach = %+v, want ready + sandbox-provenance label", c)
	}
	if !hasCheck(g, "newco") {
		t.Errorf("expected the new pack's own integration present too: %+v", g)
	}
}

// TestMCPHostGlobalContext: without a workspace sandbox context doctor reports
// registration/auth plus configured-to-preload — never attachment.
func TestMCPHostGlobalContext(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}

	t.Run("no context -> no attachment checks, preload note", func(t *testing.T) {
		g := mcpGroupWith(cfg, mcpFake().env(), "slack\n", true, true, nil, noCtx)
		if hasCheck(g, "slack attachment") {
			t.Errorf("host-global must not emit an attachment check: %+v", g)
		}
		c := findCheck(t, g, "attachment")
		if !c.note || !strings.Contains(c.detail, "preload at sandbox create") {
			t.Errorf("host-global note = %+v, want a preload-at-create statement", c)
		}
	})

	t.Run("sandbox positively absent -> not-created-yet note, no attachment", func(t *testing.T) {
		f := mcpFake()
		f.output["sbx ls"] = "pi-stack-other  running  /elsewhere\n"
		env, _ := receiptEnv(t, f, "/home/u/proj")
		ctx := resolveMCPSandboxContext(env)
		if ctx.mode != mcpAttachSandboxAbsent {
			t.Fatalf("ctx mode = %v, want mcpAttachSandboxAbsent", ctx.mode)
		}
		g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, ctx)
		if hasCheck(g, "slack attachment") {
			t.Errorf("absent sandbox must not emit an attachment check: %+v", g)
		}
		c := findCheck(t, g, "attachment")
		if !c.note || !strings.Contains(c.detail, "pi-stack-proj not created yet") {
			t.Errorf("absent-sandbox note = %+v", c)
		}
	})
}

// TestMCPRemoteAuth covers the native OAuth axis for remote servers: outcomes
// only from a bounded `sbx mcp auth status <name>` probe.
func TestMCPRemoteAuth(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"}

	authCase := func(out string, err error) check {
		f := mcpFake()
		env := f.env()
		env.probe = func(name string, args ...string) (string, bool, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if key == "sbx mcp auth status notion" {
				return out, false, err
			}
			if o, ok := f.output[key]; ok {
				return o, false, nil
			}
			return "", false, fmt.Errorf("no fake output for %q", key)
		}
		g := mcpGroupWith(cfg, env, "notion\n", true, true, nil, noCtx)
		return findCheck(t, g, "notion")
	}

	if c := authCase("notion: authorized\n", nil); c.result() != verdictReady || !strings.Contains(c.detail, "authorized") {
		t.Errorf("authorized = %+v, want ready", c)
	}
	if c := authCase("notion: 401 unauthorized\n", nil); c.result() != verdictTodo || c.todo != "pi-stack mcp auth notion" {
		t.Errorf("bare 401 = %+v, want auth TODO `pi-stack mcp auth notion`", c)
	}
	if c := authCase("notion: not authenticated\n", nil); c.result() != verdictTodo || c.todo != "pi-stack mcp auth notion" {
		t.Errorf("not-authenticated = %+v, want auth TODO", c)
	}
	if c := authCase("403 forbidden: access denied by org policy\n", fmt.Errorf("exit status 1")); c.result() != verdictDenied {
		t.Errorf("explicit policy denial = %+v, want denied", c)
	}
	if c := authCase("dial tcp: connection refused\n", fmt.Errorf("exit status 1")); c.result() != verdictUnverifiable {
		t.Errorf("network failure = %+v, want unverifiable", c)
	}
	if c := authCase("gibberish\n", nil); c.result() != verdictUnverifiable {
		t.Errorf("unparseable status = %+v, want unverifiable (never a guess)", c)
	}

	// Timeout: bounded probe deadline hit -> unverifiable.
	f := mcpFake()
	env := f.env()
	env.probe = func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == "sbx mcp auth status notion" {
			return "", true, fmt.Errorf("context deadline exceeded")
		}
		if o, ok := f.output[key]; ok {
			return o, false, nil
		}
		return "", false, fmt.Errorf("no fake output for %q", key)
	}
	g := mcpGroupWith(cfg, env, "notion\n", true, true, nil, noCtx)
	if c := findCheck(t, g, "notion"); c.result() != verdictUnverifiable || !strings.Contains(c.detail, "timed out") {
		t.Errorf("auth timeout = %+v, want unverifiable + timed out", c)
	}
}

// TestMCPLocalNeverOAuthChecked: a confirmed local stdio server must never hit
// the native OAuth probe — there is no control-plane auth for it.
func TestMCPLocalNeverOAuthChecked(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	f := mcpFake()
	env := f.env()
	inner := env.run
	env.run = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "auth" {
			t.Fatalf("local stdio server hit the native OAuth check: sbx %v", args)
		}
		return inner(name, args...)
	}
	g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, noCtx)
	findCheck(t, g, "slack") // presence is enough; the fatal above is the assertion
}

// TestMCPUnknownClassificationFailsClosed: when the local-name set cannot be
// established, doctor must not guess — no probe, no exec, no repair command.
func TestMCPUnknownClassificationFailsClosed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"mystery"}
	f := fakeEnv{
		present: map[string]bool{"sbx": true}, // hostBin unresolvable
		output: map[string]string{
			"sbx mcp get mystery": "name: mystery\ncommand: /bin/rm -rf /\n",
		},
	}
	env := f.env()
	inner := env.run
	env.run = func(name string, args ...string) (string, error) {
		if name == "/bin/rm" {
			t.Fatalf("unknown-classification server was exec'd: %s %v", name, args)
		}
		return inner(name, args...)
	}
	g := mcpGroupWith(cfg, env, "mystery\n", true, true, nil, noCtx)
	c := findCheck(t, g, "mystery")
	if c.result() != verdictUnverifiable {
		t.Errorf("unknown classification = %+v, want unverifiable", c)
	}
	if c.todo != "" {
		t.Errorf("unknown classification must not recommend a repair command: %q", c.todo)
	}
	if len((&report{groups: []group{g}}).todos()) != 0 {
		t.Errorf("unknown classification leaked a TODO: %v", (&report{groups: []group{g}}).todos())
	}
}

// TestMCPStaleAndUnknownConfigKeys: retired mcp_static/mcp_dynamic keys are a
// verified OPTIONAL leftover (never blocking); unknown keys are softer
// unverifiable info.
func TestMCPStaleAndUnknownConfigKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	toml := "mcp = [\"slack\"]\nmcp_static = [\"slack\"]\nmcp_dynamic = [\"notion\"]\nfroopy = true\n"
	if err := os.WriteFile(path, []byte(toml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	g := mcpGroupWith(cfg, mcpFake().env(), "slack\n", true, true, nil, noCtx)

	for _, k := range []string{"mcp_static", "mcp_dynamic"} {
		c := findCheck(t, g, "config "+k)
		if c.result() != verdictTodo || c.req() != requirementOptional {
			t.Errorf("retired key %s = %+v, want an optional verified todo", k, c)
		}
		if !strings.Contains(c.detail, "retired") || !strings.Contains(c.detail, "next `pi-stack config set`") {
			t.Errorf("retired key %s detail should say retired+ignored and that the next mutation drops it: %q", k, c.detail)
		}
	}
	c := findCheck(t, g, "config froopy")
	if c.result() != verdictUnverifiable {
		t.Errorf("unknown key = %+v, want softer unverifiable info", c)
	}
	if r := (&report{groups: []group{g}}); r.blocking() {
		t.Errorf("stale/unknown config keys must never block doctor's exit code")
	}
}

// TestMCPProbeTimeoutIsUnverifiable: a hung registered command hits the
// bounded probe deadline and reads as unverifiable — never a wedged doctor,
// never a fabricated failure.
func TestMCPProbeTimeoutIsUnverifiable(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	regCmd := "/usr/local/bin/pi-stack-host mcp slack"
	f := mcpFake()
	f.output["sbx mcp get slack"] = "name: slack\ncommand: " + regCmd + "\n"
	env := f.env()
	env.probe = func(name string, args ...string) (string, bool, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == regCmd+" --list-tools" {
			return "", true, fmt.Errorf("context deadline exceeded")
		}
		if o, ok := f.output[key]; ok {
			return o, false, nil
		}
		return "", false, fmt.Errorf("no fake output for %q", key)
	}
	g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, noCtx)
	c := findCheck(t, g, "slack")
	if c.result() != verdictUnverifiable || !strings.Contains(c.detail, "timed out") {
		t.Errorf("probe timeout = %+v, want unverifiable + timed out", c)
	}
}

// TestMCPCanonicalExecutableGate: a registered argv is exec'd ONLY when its
// executable tokens match the canonical resolvers, and what is exec'd is the
// RESOLVER's token — never the registered spelling — so there is no
// check-then-exec (symlink swap) window.
func TestMCPCanonicalExecutableGate(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}

	t.Run("look-alike pi-stack-host path is never exec'd", func(t *testing.T) {
		f := mcpFake()
		f.output["sbx mcp get slack"] = "name: slack\ncommand: /tmp/malicious/pi-stack-host mcp slack\n"
		env := f.env()
		inner := env.run
		env.run = func(name string, args ...string) (string, error) {
			if strings.HasPrefix(name, "/tmp/malicious/") {
				t.Fatalf("doctor exec'd a look-alike pi-stack-host: %s %v", name, args)
			}
			return inner(name, args...)
		}
		g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictUnverifiable || !strings.Contains(c.detail, "never executed") {
			t.Errorf("look-alike host binary = %+v, want unverifiable + never-executed note", c)
		}
	})

	t.Run("registered spelling is normalized to the resolver token (no TOCTOU)", func(t *testing.T) {
		// Clean-equal but differently spelled registration: the exec'd argv[0]
		// must be the resolver's canonical token, not the registered spelling —
		// a swap hook planted on the registered path never runs.
		reg := "/usr/local/bin/../bin/pi-stack-host mcp slack"
		f := mcpFake()
		f.output["sbx mcp get slack"] = "name: slack\ncommand: " + reg + "\n"
		var execd string
		env := f.env()
		inner := env.run
		env.run = func(name string, args ...string) (string, error) {
			if strings.HasSuffix(name, "pi-stack-host") && len(args) > 0 && args[len(args)-1] == "--list-tools" {
				execd = name
				return "slack_search\n", nil
			}
			return inner(name, args...)
		}
		g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictReady || !strings.Contains(c.detail, "spawns 1 tool") {
			t.Errorf("normalized spawn = %+v, want ready with tool count", c)
		}
		if execd != "/usr/local/bin/pi-stack-host" {
			t.Errorf("exec'd token = %q, want the RESOLVER's canonical path, never the registered spelling %q", execd, reg)
		}
	})

	t.Run("op-wrapped: look-alike op binary is never exec'd", func(t *testing.T) {
		f := mcpFake()
		f.present["op"] = true // lookPath("op") -> /usr/bin/op
		f.output["sbx mcp get slack"] = "name: slack\ncommand: /tmp/op run --env-file=/x -- /usr/local/bin/pi-stack-host mcp slack\n"
		env := f.env()
		inner := env.run
		env.run = func(name string, args ...string) (string, error) {
			if name == "/tmp/op" {
				t.Fatalf("doctor exec'd a look-alike op: %s %v", name, args)
			}
			return inner(name, args...)
		}
		g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictUnverifiable || !strings.Contains(c.detail, "never executed") {
			t.Errorf("look-alike op = %+v, want unverifiable + never-executed note", c)
		}
	})

	t.Run("op-wrapped canonical spawn execs the canonical op token", func(t *testing.T) {
		f := mcpFake()
		f.present["op"] = true
		// The wrapper must be the exact launcher grammar against the RESOLVED
		// op-refs path (PI_STACK_CONFIG's dir) — anything else is rejected.
		f.envVars = map[string]string{"PI_STACK_CONFIG": "/x/config.toml"}
		f.statFile = map[string]bool{"/x/op-refs.env": true}
		reg := "/usr/bin/op run --no-masking --env-file=/x/op-refs.env -- /usr/local/bin/pi-stack-host mcp slack"
		f.output["sbx mcp get slack"] = "name: slack\ncommand: " + reg + "\n"
		var execd string
		env := f.env()
		inner := env.run
		env.run = func(name string, args ...string) (string, error) {
			if len(args) > 0 && args[len(args)-1] == "--list-tools" {
				execd = name
				return "slack_search\nslack_post\n", nil
			}
			return inner(name, args...)
		}
		g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, noCtx)
		c := findCheck(t, g, "slack")
		if c.result() != verdictReady || !strings.Contains(c.detail, "spawns 2 tools") {
			t.Errorf("op-wrapped spawn = %+v, want ready", c)
		}
		if execd != "/usr/bin/op" {
			t.Errorf("exec'd wrapper token = %q, want canonical /usr/bin/op", execd)
		}
	})

	t.Run("foreign wrapper with -- is never unwrapped or exec'd", func(t *testing.T) {
		f := mcpFake()
		f.output["sbx mcp get slack"] = "name: slack\ncommand: /tmp/evil -- /usr/local/bin/pi-stack-host mcp slack\n"
		env := f.env()
		inner := env.run
		env.run = func(name string, args ...string) (string, error) {
			if name == "/tmp/evil" {
				t.Fatalf("doctor exec'd a foreign wrapper: %s %v", name, args)
			}
			return inner(name, args...)
		}
		g := mcpGroupWith(cfg, env, "slack\n", true, true, nil, noCtx)
		if c := findCheck(t, g, "slack"); c.result() != verdictUnverifiable {
			t.Errorf("foreign wrapper = %+v, want unverifiable", c)
		}
	})
}

// TestMCPNoRetiredWording: shipping output must not carry the retired
// dynamic-discovery vocabulary.
func TestMCPNoRetiredWording(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack", "notion", "linear"}
	f := mcpFake()
	f.output["sbx mcp auth status notion"] = "authorized"
	g := mcpGroupWith(cfg, f.env(), "slack\nnotion\n", true, true, nil, noCtx)
	for _, c := range g.checks {
		joined := strings.ToLower(c.label + " " + c.detail + " " + c.todo + " " + c.evidence)
		for _, banned := range []string{"dynamic discovery", "mcp-find", "attach-on-run"} {
			if strings.Contains(joined, banned) {
				t.Errorf("shipping output carries retired wording %q: %+v", banned, c)
			}
		}
	}
}

// TestMCPCatalogNamesMatchShippedBundle anti-drift-parses the shipped catalog
// bundle JSON against mcpCatalogNames, so the classifier can never silently
// diverge from what `pi-stack mcp bundle` actually registers.
func TestMCPCatalogNamesMatchShippedBundle(t *testing.T) {
	// The bundle lives at the repo root; walk up from the package dir.
	path := filepath.Join("..", "..", "..", "..", "config", "mcp-catalog.bundle.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("shipped bundle not found at %s: %v", path, err)
	}
	for name := range mcpCatalogNames {
		if !strings.Contains(string(b), `"`+name+`"`) {
			t.Errorf("mcpCatalogNames has %q but the shipped bundle does not", name)
		}
	}
	for _, name := range []string{"notion", "atlassian", "granola"} {
		if !mcpCatalogNames[name] {
			t.Errorf("shipped bundle server %q missing from mcpCatalogNames", name)
		}
	}
}
