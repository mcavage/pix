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

// gogAuthHelpCurrentSetup is the current gog CLI's `gog auth --help` output
// exposing the one-shot `setup` subcommand.
const gogAuthHelpCurrentSetup = `Manage gog authorization.

Available Commands:
  add          add an authorized account
  credentials  register an OAuth Desktop client
  doctor       diagnose auth health
  login        authorize an account interactively
  setup        one-shot: register credentials + authorize + login
  status       report auth status
`

// gogAuthHelpCurrentTwoStep is a current gog CLI that has NOT yet grown the
// one-shot `setup` subcommand, only `credentials` + `add`.
const gogAuthHelpCurrentTwoStep = `Manage gog authorization.

Available Commands:
  add          add an authorized account
  credentials  register an OAuth Desktop client
  doctor       diagnose auth health
  status       report auth status
`

// gogAuthHelpLegacy mirrors the OLDER repo docs' surface: add-client + login,
// no setup/credentials/add.
const gogAuthHelpLegacy = `Manage gog authorization.

Available Commands:
  add-client   register an OAuth Desktop client
  doctor       diagnose auth health
  login        authorize an account interactively
  status       report auth status
`

// gogAuthHelpUnsupported has none of the known subcommand surfaces.
const gogAuthHelpUnsupported = `Manage gog authorization.

Available Commands:
  doctor       diagnose auth health
  status       report auth status
`

// gogTestEnv builds a hermetic shellEnv + a recorder of every "interactive"
// command gogSetup would otherwise hand real stdio to (the browser-opening
// auth steps), so tests never touch a real gog binary or terminal.
type gogTestEnv struct {
	present    map[string]bool
	output     map[string]string // "cmd arg arg" -> combined output
	outputErr  map[string]bool   // "cmd arg arg" -> true if that call errors
	statFile   map[string]bool
	interCalls *[][]string // recorded runInteractive invocations
	interErr   error       // error runInteractive returns (nil = success)
}

func (g gogTestEnv) env() shellEnv {
	calls := g.interCalls
	if calls == nil {
		calls = &[][]string{}
	}
	return shellEnv{
		lookPath: func(name string) (string, error) {
			if g.present[name] {
				return "/usr/bin/" + name, nil
			}
			return "", fmt.Errorf("exec: %q not found", name)
		},
		run: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if g.outputErr[key] {
				return g.output[key], fmt.Errorf("exit status 1")
			}
			if out, ok := g.output[key]; ok {
				return out, nil
			}
			return "", fmt.Errorf("no fake output for %q", key)
		},
		getenv:   os.Getenv,
		statFile: func(path string) bool { return g.statFile[path] },
		runInteractive: func(name string, args ...string) error {
			*calls = append(*calls, append([]string{name}, args...))
			return g.interErr
		},
	}
}

// gogSetupTestCfg points config.Load()/Save() at a fresh temp file for the
// duration of one test.
func gogSetupTestCfg(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
}

func gogCredFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "client.json")
	if err := os.WriteFile(p, []byte(`{"installed":{"client_id":"fake"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGogSetup_CurrentOneShotRoute(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "op": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile:   map[string]bool{cred: true},
		interCalls: &calls,
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	if len(calls) != 1 {
		t.Fatalf("expected 1 interactive call (one-shot setup), got %d: %v", len(calls), calls)
	}
	want := []string{"gog", "auth", "setup", "you@example.com", "--credentials", cred, "--login"}
	if strings.Join(calls[0], " ") != strings.Join(want, " ") {
		t.Errorf("interactive call = %v, want %v", calls[0], want)
	}
	if strings.Contains(out.String(), `"client_id"`) || strings.Contains(out.String(), "fake") {
		t.Errorf("output must never contain credential file contents: %q", out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.GogAccount != "you@example.com" {
		t.Errorf("GogAccount = %q, want you@example.com", cfg.GogAccount)
	}
	found := false
	for _, m := range cfg.MCP {
		if m == "gog" {
			found = true
		}
	}
	if !found {
		t.Errorf("cfg.MCP = %v, want gog present", cfg.MCP)
	}
}

func TestGogSetup_CurrentTwoStepRoute(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentTwoStep,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile:   map[string]bool{cred: true},
		interCalls: &calls,
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 interactive calls (credentials + add), got %d: %v", len(calls), calls)
	}
	if strings.Join(calls[0], " ") != "gog auth credentials "+cred {
		t.Errorf("call[0] = %v", calls[0])
	}
	if strings.Join(calls[1], " ") != "gog auth add you@example.com" {
		t.Errorf("call[1] = %v", calls[1])
	}
}

func TestGogSetup_LegacyFallbackRoute(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpLegacy,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile:   map[string]bool{cred: true},
		interCalls: &calls,
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	if len(calls) != 2 {
		t.Fatalf("expected 2 interactive calls (add-client + login), got %d: %v", len(calls), calls)
	}
	if strings.Join(calls[0], " ") != "gog auth add-client "+cred {
		t.Errorf("call[0] = %v", calls[0])
	}
	if strings.Join(calls[1], " ") != "gog --account you@example.com auth login" {
		t.Errorf("call[1] = %v", calls[1])
	}
}

func TestGogSetup_UnsupportedCLI_NoObsoleteExec(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpUnsupported,
			"gog --version":   "gog version 0.1.0",
		},
		statFile:   map[string]bool{cred: true},
		interCalls: &calls,
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error for an unsupported gog CLI surface")
	}
	if len(calls) != 0 {
		t.Errorf("must not exec any obsolete/guessed command, got %v", calls)
	}
	if !strings.Contains(out.String(), "0.1.0") {
		t.Errorf("expected installed version in guidance, got %q", out.String())
	}
	if !strings.Contains(out.String(), "upgrade") {
		t.Errorf("expected upgrade guidance, got %q", out.String())
	}
}

func TestGogSetup_MissingGogCLI(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{present: map[string]bool{}, statFile: map[string]bool{cred: true}}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when gog is not installed")
	}
	if !strings.Contains(out.String(), "brew install gog") && !strings.Contains(out.String(), "gogcli.sh") {
		t.Errorf("expected exact install guidance, got %q", out.String())
	}
}

func TestGogSetup_MissingCredentialsFile(t *testing.T) {
	gogSetupTestCfg(t)
	ge := gogTestEnv{present: map[string]bool{"gog": true}, statFile: map[string]bool{}}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: "/no/such/client.json"}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error for a missing credentials file")
	}
	if strings.Contains(err.Error(), "client_id") {
		t.Errorf("error must never quote file contents")
	}
}

func TestGogSetup_AuthDoctorFails(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
		},
		outputErr: map[string]bool{
			"gog --account you@example.com auth doctor --check": true,
		},
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when auth doctor --check fails")
	}
	cfg, _ := config.Load()
	if cfg != nil && cfg.GogAccount != "" {
		t.Errorf("must not persist gog_account when auth verification failed, got %q", cfg.GogAccount)
	}
}

func TestGogSetup_ZeroHeadlessToolsFailsWithGuidance(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	dir := t.TempDir()
	refs := filepath.Join(dir, "op-refs.env")
	if err := os.WriteFile(refs, []byte("GOG_ACCOUNT=you@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	// resolveOpRefs looks relative to PI_STACK_CONFIG's dir first.
	if err := os.MkdirAll(filepath.Join(dir, "cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	refs = filepath.Join(dir, "cfg", "op-refs.env")
	if err := os.WriteFile(refs, []byte("GOG_ACCOUNT=you@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "op": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
			// headless probe (op run --env-file=... -- gog ... mcp --list-tools)
			// returns EMPTY output => zero tools.
			"op run --env-file=" + refs + " -- gog --account you@example.com mcp --list-tools": "",
		},
		statFile: map[string]bool{cred: true, refs: true},
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when headless returns zero tools")
	}
	if !strings.Contains(out.String(), "GOG_KEYRING_PASSWORD") {
		t.Errorf("expected exact op-refs/keyring guidance, got %q", out.String())
	}
	cfg, _ := config.Load()
	if cfg != nil {
		found := false
		for _, m := range cfg.MCP {
			if m == "gog" {
				found = true
			}
		}
		if found {
			t.Errorf("must not register gog when headless verification failed")
		}
	}
}

func TestGogSetup_IdempotentAddMCPWhenAccountUnchanged(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	// Pre-seed config with the SAME account already set (simulating a
	// re-run), but gog missing from cfg.MCP (e.g. someone removed it, or it
	// was never added due to the fixed idempotency bug).
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("you@example.com")
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range got.MCP {
		if m == "gog" {
			found = true
		}
	}
	if !found {
		t.Errorf("re-running healthy setup with an UNCHANGED account must still add gog to cfg.MCP; got %v", got.MCP)
	}
}

func TestGogSetup_RegistrationFailureReported(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
			// no fake "sbx mcp add gog ..." output => registerServers' env.run
			// call for it returns an error, exercising the registration-failure
			// path end to end.
		},
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("expected an error when sbx registration fails")
	}
}

func TestGogSetup_NoCredentialContentReads(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	readFileCalled := false
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile: map[string]bool{cred: true},
	}
	env := ge.env()
	env.readFile = func(path string) (string, error) {
		readFileCalled = true
		return "", fmt.Errorf("must not be called")
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(env, opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	if readFileCalled {
		t.Error("gogSetup must never read the credentials file's contents")
	}
}

func TestGogSetup_PromptsOnTTYForMissingAccountAndCredentials(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	in := strings.NewReader("you@example.com\n" + cred + "\n")
	opts := gogSetupOpts{}
	if err := gogSetup(ge.env(), opts, in, &out, true); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
}

// TestGogSetup_BufferedReaderDeliversBothPromptedValues is QA finding #3: feed
// BOTH the account and credentials lines through one fully-buffered io.Reader
// (TTY=true, no flags), and verify BOTH prompted values actually reach the
// selected auth command's argv — not merely that gogSetup returns no error.
//
// This targets a previously unguarded integration:
// TestGogSetup_PromptsOnTTYForMissingAccountAndCredentials (above) already
// prompts for both values on a fully-buffered strings.Reader, but it never
// wires gogTestEnv.interCalls, so it could not have caught a regression where
// gogSetup silently ran the auth command with a wrong/empty credentials path
// (any error from a wrong account WOULD have failed that test via the
// `sbx`-shaped exact-string run() map, but a wrong/empty CREDENTIALS value
// only shows up in the recorded runInteractive argv, which no prior test
// captured). gogSetup deliberately shares ONE bufio.Reader across both
// prompts for exactly this reason (see its doc comment): promptLine's
// per-call `bufio.NewReader(sio.in)` would silently drop the second answer
// here, because the first call's internal read-ahead can consume the whole
// buffered input (including the credentials line) from the underlying
// strings.Reader, leaving nothing for a second, freshly-constructed
// bufio.Reader to read.
func TestGogSetup_BufferedReaderDeliversBothPromptedValues(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile:   map[string]bool{cred: true},
		interCalls: &calls,
	}
	var out bytes.Buffer
	// ONE fully-buffered io.Reader carrying BOTH answers: a real strings.Reader
	// delivers its entire contents on the first Read, exactly the shape that
	// exposes the per-call-bufio.Reader bug (a second bufio.NewReader wrapping
	// the same, now-drained, underlying reader would see EOF).
	in := strings.NewReader("you@example.com\n" + cred + "\n")
	opts := gogSetupOpts{} // no flags — both values MUST come from the reader
	if err := gogSetup(ge.env(), opts, in, &out, true); err != nil {
		t.Fatalf("gogSetup: %v\n--- output ---\n%s", err, out.String())
	}
	if len(calls) == 0 {
		t.Fatal("expected at least one runInteractive (auth) call to be recorded")
	}
	// The selected route's FIRST command carries the auth argv gogSetup built
	// from the two prompted values — assert both landed in it correctly.
	got := strings.Join(calls[0], " ")
	if !strings.Contains(got, "you@example.com") {
		t.Errorf("expected the prompted account to reach the auth command, got %q", got)
	}
	if !strings.Contains(got, cred) {
		t.Errorf("expected the prompted credentials path to reach the auth command, got %q", got)
	}
}

func TestGogSetup_NonInteractiveMissingAccountFails(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{present: map[string]bool{"gog": true}, statFile: map[string]bool{cred: true}}
	var out bytes.Buffer
	opts := gogSetupOpts{credentials: cred}
	if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err == nil {
		t.Fatal("expected an error when --account is missing and not a TTY")
	}
}

func TestGogSetup_PrintsAttachModeGuidance(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true},
		output: map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		},
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	opts := gogSetupOpts{account: "you@example.com", credentials: cred}
	if err := gogSetup(ge.env(), opts, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("gogSetup: %v", err)
	}
	if !strings.Contains(out.String(), "dynamically discoverable") {
		t.Errorf("expected dynamic-discovery note, got %q", out.String())
	}
	if !strings.Contains(out.String(), "pi-stack mcp load gog") {
		t.Errorf("expected the mcp load follow-up, got %q", out.String())
	}
}

// --- CLI dispatch / help ---

func TestRunGogCmd_HelpAndUnknownSubcommand(t *testing.T) {
	if !wantsHelp([]string{"-h"}) {
		t.Fatal("sanity")
	}
	if _, ok := verbUsage("gog"); !ok {
		t.Error("verbUsage(gog) should be known")
	}
	if !knownVerbs["gog"] {
		t.Error(`knownVerbs["gog"] should be true`)
	}
}

func TestParseGogSetupArgs(t *testing.T) {
	opts, err := parseGogSetupArgs([]string{"--account", "you@example.com", "--credentials", "/tmp/c.json", "--yes"})
	if err != nil {
		t.Fatalf("parseGogSetupArgs: %v", err)
	}
	if opts.account != "you@example.com" || opts.credentials != "/tmp/c.json" || !opts.assumeYes {
		t.Errorf("parsed = %+v", opts)
	}
	if _, err := parseGogSetupArgs([]string{"--account"}); err == nil {
		t.Error("expected error for --account with no value")
	}
	if _, err := parseGogSetupArgs([]string{"--bogus"}); err == nil {
		t.Error("expected error for unknown flag")
	}
}

func TestChooseGogAuthRoute(t *testing.T) {
	cases := []struct {
		help string
		want string
		ok   bool
	}{
		{gogAuthHelpCurrentSetup, "setup", true},
		{gogAuthHelpCurrentTwoStep, "credentials+add", true},
		{gogAuthHelpLegacy, "add-client+login (legacy)", true},
		{gogAuthHelpUnsupported, "", false},
	}
	for _, c := range cases {
		route, ok := chooseGogAuthRoute(c.help)
		if ok != c.ok {
			t.Errorf("chooseGogAuthRoute(%q) ok = %v, want %v", c.help, ok, c.ok)
			continue
		}
		if ok && route.name != c.want {
			t.Errorf("chooseGogAuthRoute(%q) = %q, want %q", c.help, route.name, c.want)
		}
	}
}
