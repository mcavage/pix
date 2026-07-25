package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
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

// The following are the SUBCOMMAND-level `gog auth <verb> --help` outputs
// gogAuthRouteCapable probes (R1-02/R1-12): each step's OWN help text, not
// just the top-level `gog auth --help` listing. The "Readonly" variants
// advertise every flag gogSetup requires (including "--readonly" on any step
// that performs an OAuth grant); the "NoReadonly" variants are missing it, to
// exercise the capability-probe failure path.
const gogAuthSetupHelpReadonly = `usage: gog auth setup <account> [flags]

Register credentials, authorize, and log in, in one step.

Flags:
      --credentials string   path to your OAuth Desktop client JSON
      --login                authorize interactively after registering
      --readonly              request read-only OAuth scopes
`

const gogAuthSetupHelpNoReadonly = `usage: gog auth setup <account> [flags]

Flags:
      --credentials string   path to your OAuth Desktop client JSON
      --login                authorize interactively after registering
`

const gogAuthCredentialsHelp = `usage: gog auth credentials <path>

Register an OAuth Desktop client.
`

const gogAuthAddHelpReadonly = `usage: gog auth add <account> [flags]

Flags:
      --readonly   request read-only OAuth scopes
`

const gogAuthAddHelpNoReadonly = `usage: gog auth add <account> [flags]

(no flags)
`

const gogAuthAddClientHelp = `usage: gog auth add-client <path>

Register an OAuth Desktop client.
`

const gogAuthLoginHelpReadonly = `usage: gog auth login [flags]

Flags:
      --readonly   request read-only OAuth scopes
`

const gogAuthLoginHelpNoReadonly = `usage: gog auth login [flags]

(no flags)
`

// gogAuthCapabilityFixtures returns the exact `gog auth <verb> --help` output
// entries needed for gogAuthRouteCapable to accept the named route, keyed
// exactly as gogTestEnv.output expects ("gog auth <verb> --help"). readonly
// selects between the flag-complete and flag-missing variant of every step
// that performs an OAuth grant.
func gogAuthCapabilityFixtures(route string, readonly bool) map[string]string {
	switch route {
	case "setup":
		if readonly {
			return map[string]string{"gog auth setup --help": gogAuthSetupHelpReadonly}
		}
		return map[string]string{"gog auth setup --help": gogAuthSetupHelpNoReadonly}
	case "credentials+add":
		addHelp := gogAuthAddHelpReadonly
		if !readonly {
			addHelp = gogAuthAddHelpNoReadonly
		}
		return map[string]string{
			"gog auth credentials --help": gogAuthCredentialsHelp,
			"gog auth add --help":         addHelp,
		}
	case "add-client+login (legacy)":
		loginHelp := gogAuthLoginHelpReadonly
		if !readonly {
			loginHelp = gogAuthLoginHelpNoReadonly
		}
		return map[string]string{
			"gog auth add-client --help": gogAuthAddClientHelp,
			"gog auth login --help":      loginHelp,
		}
	}
	return nil
}

// mergeOutputs shallow-merges any number of "cmd" -> output maps into one, so
// tests can compose a route's capability-help fixtures with its other faked
// output without repeating every entry by hand.
func mergeOutputs(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

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
	// sbxRegisterOK makes an otherwise-unfixtured `sbx mcp add <name> ...` call
	// (registerServers' actual registration exec) succeed by default, so a test
	// that only cares about gogSetup's OWN verification logic doesn't need to
	// hand-construct the exact addArgs argv just to let registration through. A
	// test that WANTS to exercise a registration failure (e.g.
	// TestGogSetup_RegistrationFailureReported) leaves this false.
	sbxRegisterOK bool
	// fileModeOverride lets a test (R2-05) drive env.fileMode's answer for a
	// specific path directly (e.g. a FIFO/socket/device mode, or "not found").
	// A path absent here falls back to env().fileMode's default: a plain
	// regular-file mode iff statFile[path] is true, so every existing test that
	// only sets statFile for its credentials path keeps working unchanged.
	fileModeOverride map[string]os.FileMode
}

// gogBareHeadlessKey is the exact bare (no op wrapper) hardened headless probe
// key gogTestEnv's canonical "/usr/bin/gog" lookPath answer produces (R1-06):
// the same hardened invocation mcp.go registers, plus "--list-tools".
func gogBareHeadlessKey(acct string) string {
	return strings.Join(gogHardenedArgv("/usr/bin/gog", acct), " ") + " --list-tools"
}

// gogBareHeadlessFixture is a one-entry output map making the bare headless
// probe (R1-06, when op/op-refs are unavailable) report a healthy, non-empty
// tool list for acct.
func gogBareHeadlessFixture(acct string) map[string]string {
	return map[string]string{gogBareHeadlessKey(acct): "gmail_search\ncalendar_events\n"}
}

// gogWrappedHeadlessKey is the exact op-wrapped, hardened headless probe key
// (op + opRefs both present): the same `op run --no-masking --env-file=<refs>`
// wrapper + hardened gog invocation mcpRegistrar/gogRegisteredArgv actually
// register, plus "--list-tools" (finding #2 -- gogSetup's own verification
// must probe this EXACT command, never a lighter reconstruction).
func gogWrappedHeadlessKey(acct, opRefs string) string {
	return strings.Join(gogRegisteredArgv("/usr/bin/gog", "/usr/bin/op", opRefs, acct), " ") + " --list-tools"
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
			if g.sbxRegisterOK && name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "add" {
				return "", nil
			}
			// R2-03/R2-04 default: an otherwise-unfixtured bounded `sbx mcp ls`
			// listing (snapshotGogRegistration's presence probe) defaults to an
			// EMPTY listing (confirmed absent) — most gogSetup tests don't care
			// about a prior registration, and requiring every one of them to
			// fixture this explicitly would be pure noise. A test that DOES care
			// sets g.output["sbx mcp ls"] explicitly, which the check above
			// already wins over this default.
			if name == "sbx" && len(args) == 2 && args[0] == "mcp" && args[1] == "ls" {
				return "", nil
			}
			return "", fmt.Errorf("no fake output for %q", key)
		},
		getenv:   os.Getenv,
		statFile: func(path string) bool { return g.statFile[path] },
		// R2-05: fileMode drives the credentials regular-file check. A test-set
		// override wins; otherwise a path marked present in statFile defaults to
		// a plain regular-file mode (0o600) so every pre-existing statFile-only
		// fixture keeps behaving as a valid regular file.
		fileMode: func(path string) (os.FileMode, bool) {
			if g.fileModeOverride != nil {
				if m, ok := g.fileModeOverride[path]; ok {
					return m, true
				}
			}
			if g.statFile[path] {
				return 0o600, true
			}
			return 0, false
		},
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
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
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
		present: map[string]bool{"gog": true, "op": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
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
	if len(calls) != 1 {
		t.Fatalf("expected 1 interactive call (one-shot setup), got %d: %v", len(calls), calls)
	}
	// R1-02: the OAuth-granting step always carries --readonly.
	want := []string{"gog", "auth", "setup", "you@example.com", "--credentials", cred, "--login", "--readonly"}
	gotCall := calls[len(calls)-1]
	if len(gotCall) == len(want) && len(gotCall) > 5 && strings.Contains(gotCall[5], "pix-gog-") {
		want[5] = gotCall[5]
	}
	if strings.Join(gotCall, " ") != strings.Join(want, " ") {
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
		if m == gwServerName {
			found = true
		}
	}
	if !found {
		t.Errorf("cfg.MCP = %v, want google-workspace present", cfg.MCP)
	}
}

func TestGogSetup_CurrentTwoStepRoute(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("credentials+add", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentTwoStep,
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
	if len(calls) != 2 {
		t.Fatalf("expected 2 interactive calls (credentials + add), got %d: %v", len(calls), calls)
	}
	if !strings.HasPrefix(strings.Join(calls[0], " "), "gog auth credentials /tmp/pix-gog-") {
		t.Errorf("call[0] = %v", calls[0])
	}
	// R1-02: the OAuth-granting `auth add` step always carries --readonly.
	if strings.Join(calls[1], " ") != "gog auth add you@example.com --readonly" {
		t.Errorf("call[1] = %v", calls[1])
	}
}

func TestGogSetup_LegacyFallbackRoute(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	var calls [][]string
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("add-client+login (legacy)", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpLegacy,
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
	if len(calls) != 2 {
		t.Fatalf("expected 2 interactive calls (add-client + login), got %d: %v", len(calls), calls)
	}
	if !strings.HasPrefix(strings.Join(calls[0], " "), "gog auth add-client /tmp/pix-gog-") {
		t.Errorf("call[0] = %v", calls[0])
	}
	// R1-02: the OAuth-granting `auth login` step always carries --readonly.
	if strings.Join(calls[1], " ") != "gog --account you@example.com auth login --readonly" {
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
	if !strings.Contains(out.String(), gwInstallCmd) {
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
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
		}),
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
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "cfg", "config.toml"))
	// resolveOpRefs looks relative to PIX_CONFIG's dir first.
	if err := os.MkdirAll(filepath.Join(dir, "cfg"), 0o700); err != nil {
		t.Fatal(err)
	}
	refs = filepath.Join(dir, "cfg", "op-refs.env")
	if err := os.WriteFile(refs, []byte("GOG_ACCOUNT=you@example.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ge := gogTestEnv{
		// sbx present: the R2-04 preflight (sbx binary + registration snapshot)
		// runs before this test's headless-verification step, so sbx must be
		// wired for the test to reach it at all.
		present: map[string]bool{"gog": true, "op": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
			// headless probe: the EXACT hardened, op-wrapped command registration
			// will use (gogWrappedHeadlessKey/gogRegisteredArgv) returns EMPTY
			// output => zero tools.
			gogWrappedHeadlessKey("you@example.com", refs): "",
		}),
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
			if m == gwServerName {
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
	got, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range got.MCP {
		if m == gwServerName {
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
			// no fake "sbx mcp add google-workspace ..." output => registerServers' env.run
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
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		statFile:      map[string]bool{cred: true},
		sbxRegisterOK: true,
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
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		statFile:      map[string]bool{cred: true},
		sbxRegisterOK: true,
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
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), gogBareHeadlessFixture("you@example.com"), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
		}),
		statFile:      map[string]bool{cred: true},
		interCalls:    &calls,
		sbxRegisterOK: true,
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
	if !strings.Contains(got, "--credentials") || !strings.Contains(got, "pix-gog-") {
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

// (S07: every configured MCP server preloads at sandbox CREATE — there is no
// static/dynamic attach split any more, so the guidance names the preload +
// the live `mcp load` path.)
func TestGogSetup_PrintsAttachModeGuidance(t *testing.T) {
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
		t.Fatalf("gogSetup: %v", err)
	}
	if !strings.Contains(out.String(), "preloads it") {
		t.Errorf("expected the preload-at-create note, got %q", out.String())
	}
	if !strings.Contains(out.String(), "pix mcp load google-workspace") {
		t.Errorf("expected the mcp load follow-up, got %q", out.String())
	}
}

// --- CLI dispatch / help ---

func TestRunGogCmd_HelpAndUnknownSubcommand(t *testing.T) {
	if !wantsHelp([]string{"-h"}) {
		t.Fatal("sanity")
	}
	if _, ok := verbUsage("gworkspace"); !ok {
		t.Error("verbUsage(gog) should be known")
	}
	if !knownVerbs["gworkspace"] {
		t.Error(`knownVerbs["gworkspace"] should be true`)
	}
}

func TestParseGogSetupArgs(t *testing.T) {
	opts, err := parseGworkspaceSetupArgs([]string{"--account", "you@example.com", "--credentials", "/tmp/c.json", "--yes"})
	if err != nil {
		t.Fatalf("parseGworkspaceSetupArgs: %v", err)
	}
	if opts.account != "you@example.com" || opts.credentials != "/tmp/c.json" || !opts.assumeYes {
		t.Errorf("parsed = %+v", opts)
	}
	if _, err := parseGworkspaceSetupArgs([]string{"--account"}); err == nil {
		t.Error("expected error for --account with no value")
	}
	if _, err := parseGworkspaceSetupArgs([]string{"--bogus"}); err == nil {
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
