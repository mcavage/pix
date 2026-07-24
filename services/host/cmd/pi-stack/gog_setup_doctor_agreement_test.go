package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// gog_setup_doctor_agreement_test.go is QA finding #2: a hermetic,
// integration-style test that runs `pi-stack gog setup` (gogSetup) and
// `pi-stack doctor` (runDoctor) against the SAME fake config/env, and checks
// they agree on gog's headless-probe outcome.
//
// It is a genuine round trip, not two independently hand-fed fixtures: a fake
// `sbx` here REMEMBERS the argv gogSetup registers via `sbx mcp add gog …`
// and replays it for doctor's `sbx mcp get gog`, so doctor's HONEST path
// (registeredGogCommand + probeRegisteredGog) probes the EXACT command setup
// produced. The underlying `gog … mcp --list-tools` process is modeled by
// ONE canned answer shared by both gogSetup's own lighter verification probe
// (gogHeadlessOK: `op run --env-file=… -- gog --account … mcp --list-tools`)
// and doctor's honest-path probe of the fuller, hardened, ACTUALLY-registered
// command (`op run --no-masking --env-file=… -- gog --account … --gmail-no-send
// --wrap-untrusted --readonly mcp --allow-tool read --list-tools`) — two
// different exact command strings that must still describe the same real
// account, so a real gog process would answer both the same way.

// reconstructMCPAddCommand rebuilds the single command line `sbx mcp get`
// would report for a `sbx mcp add <name> --command X --args a --args b …`
// invocation: "X a b …". This is what lets the fake sbx below serve doctor's
// `sbx mcp get gog` from whatever gogSetup's registerServers call to `sbx mcp
// add gog …` actually sent — a real readback, not a hand-typed fixture.
func reconstructMCPAddCommand(addArgs []string) string {
	var cmdTok string
	var rest []string
	for i := 0; i < len(addArgs); i++ {
		switch addArgs[i] {
		case "--command":
			i++
			if i < len(addArgs) {
				cmdTok = addArgs[i]
			}
		case "--args":
			i++
			if i < len(addArgs) {
				rest = append(rest, addArgs[i])
			}
		}
	}
	return strings.Join(append([]string{cmdTok}, rest...), " ")
}

// gogPipelineFake is a stateful fake shellEnv shared by gogSetup and
// runDoctor: it plays the role of sbx/op/gog across BOTH commands, so
// doctor's honest path can read back exactly what setup registered.
type gogPipelineFake struct {
	acct       string
	opRefs     string
	cred       string
	cfgPathEnv string            // the fake $PI_STACK_CONFIG path (drives resolveOpRefs)
	registered map[string]string // name -> the argv `sbx mcp get <name>` would show
	// headlessEmpty makes every `… mcp … --list-tools` probe (both gogSetup's
	// own lighter one and doctor's fuller honest-path one) return ZERO tools —
	// the shared "underlying gog process" fact both commands observe.
	headlessEmpty bool
	interCalls    [][]string
}

func (g *gogPipelineFake) env() shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "gog" || name == "op" || name == "sbx" {
				return "/usr/bin/" + name, nil
			}
			return "", fmt.Errorf("exec: %q not found", name)
		},
		run: func(name string, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			switch {
			case name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "add":
				g.registered[args[2]] = reconstructMCPAddCommand(args[3:])
				return "", nil
			case name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "get":
				cmd, ok := g.registered[args[2]]
				if !ok {
					return "", fmt.Errorf("sbx: %s not registered", args[2])
				}
				return "name: " + args[2] + "\ncommand: " + cmd + "\n", nil
			case name == "sbx" && len(args) == 2 && args[0] == "secret" && args[1] == "ls":
				return "anthropic openai google github", nil
			case name == "sbx" && len(args) == 2 && args[0] == "mcp" && args[1] == "ls":
				if _, ok := g.registered["gog"]; ok {
					return "gog\n", nil
				}
				return "", nil
			case name == "gog" && joined == "auth --help":
				return gogAuthHelpCurrentSetup, nil
			// R1-02/R1-12: gogAuthRouteCapable probes the SELECTED route's own
			// subcommand help for its required flags (here, the one-shot "setup"
			// route's --credentials/--login/--readonly) before ever running it.
			case name == "gog" && joined == "auth setup --help":
				return gogAuthSetupHelpReadonly, nil
			case name == "gog" && joined == "--account "+g.acct+" auth doctor --check":
				return "ok", nil
			// Both gogSetup's own light headless verification (name="op", a bare
			// lookPath-free call) AND doctor's honest-path probe of the fuller
			// registered command (name="/usr/bin/op", the looked-up absolute path
			// baked into the registered argv) end in "--list-tools" and carry the
			// same account — model ONE real gog process answering both.
			case filepath.Base(name) == "op" && strings.HasPrefix(joined, "run ") &&
				strings.Contains(joined, "--account "+g.acct+" ") &&
				strings.HasSuffix(joined, "--list-tools"):
				if g.headlessEmpty {
					return "", nil
				}
				return "gmail_search\ncalendar_events\ndocs_get\n", nil
			}
			return "", fmt.Errorf("no fake output for %q %q", name, joined)
		},
		getenv: func(name string) string {
			if name == "PI_STACK_CONFIG" {
				return g.cfgPathEnv
			}
			return ""
		},
		statFile: func(path string) bool { return path == g.cred || path == g.opRefs },
		// R2-05: the credentials regular-file check now reads fileMode, not
		// statFile — report a plain regular file for the same paths.
		fileMode: func(path string) (os.FileMode, bool) {
			if path == g.cred || path == g.opRefs {
				return 0o600, true
			}
			return 0, false
		},
		dial: func(int) bool { return false }, // memory/ollama ports irrelevant here
		runInteractive: func(name string, args ...string) error {
			g.interCalls = append(g.interCalls, append([]string{name}, args...))
			return nil
		},
	}
}

func newGogPipelineFake(t *testing.T, acct string) (*gogPipelineFake, string) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	credDir := t.TempDir()
	cred := filepath.Join(credDir, "client.json")
	g := &gogPipelineFake{
		acct:       acct,
		opRefs:     filepath.Join(dir, "op-refs.env"),
		cred:       cred,
		registered: map[string]string{},
	}
	g.cfgPathEnv = cfgPath
	return g, cfgPath
}

// TestGogSetupDoctorAgreement is QA finding #2: gogSetup and runDoctor, run
// against the SAME fake sbx/op/gog state, must agree on gog's headless-probe
// outcome — both healthy, and both failed on zero tools.
func TestGogSetupDoctorAgreement(t *testing.T) {
	const acct = "you@example.com"
	g, cfgPath := newGogPipelineFake(t, acct)
	t.Setenv("PI_STACK_CONFIG", cfgPath)

	// --- Phase 1: healthy. Run `gog setup` fresh; it must succeed, register
	// gog with the fake sbx, and save the account + MCP entry. ---
	var setupOut bytes.Buffer
	err := gogSetup(g.env(), gogSetupOpts{account: acct, credentials: g.cred}, strings.NewReader(""), &setupOut, false)
	if err != nil {
		t.Fatalf("gogSetup (healthy): %v\n--- output ---\n%s", err, setupOut.String())
	}
	if !strings.Contains(setupOut.String(), "headless tools OK") {
		t.Errorf("expected gogSetup's own healthy wording, got %q", setupOut.String())
	}
	if _, ok := g.registered["gog"]; !ok {
		t.Fatalf("expected gogSetup to register gog with the fake sbx")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.GogAccount != acct {
		t.Fatalf("expected gogSetup to have saved gog_account=%s, got %q", acct, cfg.GogAccount)
	}

	r := runDoctor(cfg, g.env())
	headless := findGogCheck(t, r, "headless spawn")
	if headless.evidence != EvidenceHealthy || headless.state() != stateOK {
		t.Errorf("expected doctor's headless spawn to agree with gogSetup's healthy verification: evidence=%q state=%v detail=%q",
			headless.evidence, headless.state(), headless.detail)
	}
	if !strings.Contains(headless.detail, "exposes tools") {
		t.Errorf("expected doctor's honest-path healthy detail, got %q", headless.detail)
	}
	// Both agree on Evidence == healthy. Their WORDING is intentionally
	// different (gogSetup: "headless tools OK …", addressed at the person who
	// just ran setup; doctor: "registered command exposes tools …", addressed
	// at whoever runs doctor later against the persisted registration) — assert
	// that INTENDED distinction explicitly rather than requiring identical
	// strings.
	if strings.Contains(setupOut.String(), headless.detail) {
		t.Errorf("gogSetup's and doctor's healthy messages are expected to differ in wording (different audiences), got identical text %q", headless.detail)
	}

	// --- Phase 2: the SAME registered command later starts returning ZERO
	// tools (e.g. a keyring/headless-creds regression). Re-running `gog setup`
	// for the SAME account must now fail at headless verification (and must
	// NOT re-register, so the fake sbx's stored registration is UNCHANGED —
	// this is what lets doctor probe the exact command that setup itself just
	// rejected). ---
	g.headlessEmpty = true
	var reOut bytes.Buffer
	setupErr2 := gogSetup(g.env(), gogSetupOpts{account: acct, credentials: g.cred}, strings.NewReader(""), &reOut, false)
	if setupErr2 == nil {
		t.Fatalf("expected gogSetup to fail once the headless probe returns zero tools, output:\n%s", reOut.String())
	}
	if !strings.Contains(setupErr2.Error(), "headless verification failed") {
		t.Errorf("expected gogSetup's headless-failure wording, got %q", setupErr2.Error())
	}

	cfg2, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	r2 := runDoctor(cfg2, g.env())
	headless2 := findGogCheck(t, r2, "headless spawn")
	if headless2.evidence != EvidenceFailed || headless2.state() != stateTODO {
		t.Errorf("expected doctor's headless spawn to agree with gogSetup's failure: evidence=%q state=%v detail=%q",
			headless2.evidence, headless2.state(), headless2.detail)
	}
	if !strings.Contains(headless2.detail, "0 tools") {
		t.Errorf("expected doctor's zero-tools wording, got %q", headless2.detail)
	}
	// Again: same Evidence (failed), deliberately different exact strings
	// (gogSetup's error names the trap for the person mid-setup; doctor's TODO
	// carries the fix-it command for whoever runs doctor later) — assert that
	// explicitly rather than comparing raw strings.
	if setupErr2.Error() == headless2.detail {
		t.Errorf("gogSetup's error and doctor's TODO detail are expected to differ in wording, got identical text %q", setupErr2.Error())
	}
	if headless2.todo == "" {
		t.Errorf("expected doctor's failed headless spawn to carry a fix-it TODO")
	}
}

// findGogCheck locates a labeled check inside the gog group, failing the test
// if the group or the check is missing.
func findGogCheck(t *testing.T, r *report, label string) check {
	t.Helper()
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "gog") {
			continue
		}
		for _, c := range g.checks {
			if c.label == label {
				return c
			}
		}
	}
	t.Fatalf("doctor did not report a %q check in the gog group, groups=%+v", label, r.groups)
	return check{}
}
