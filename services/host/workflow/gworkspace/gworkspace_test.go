// gworkspace_test.go covers the PUBLIC façade in gworkspace.go: `pix
// gworkspace setup|status|disable`, the flag-gating that keeps --account/
// --credentials meaningful ONLY alongside the Google Workspace opt-in, the
// zero-tools-never-registers + register-then-save rollback guarantees
// (delegated verbatim to gog_setup.go, exercised here THROUGH the façade so a
// regression in the wiring itself would show up here even if gog_setup.go's
// own tests stayed green), and the naming contract gworkspace.go's own header
// documents: CLI noun `gworkspace`, MCP name `google-workspace` (config.GWServerName),
// config key `google_workspace_account`, "gog" appearing only in the
// dependency install/upgrade lines.
package gworkspace

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// --- ParseGworkspaceSetupArgs -------------------------------------------

func TestParseGworkspaceSetupArgs(t *testing.T) {
	o, err := ParseGworkspaceSetupArgs([]string{"--account", "a@b.com", "--credentials=/x/c.json", "--yes"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.Account != "a@b.com" || o.Credentials != "/x/c.json" || !o.AssumeYes {
		t.Errorf("parsed = %+v", o)
	}
	if _, err := ParseGworkspaceSetupArgs([]string{"--account"}); err == nil {
		t.Error("--account without a value should error")
	}
	if _, err := ParseGworkspaceSetupArgs([]string{"--bogus"}); err == nil {
		t.Error("an unknown flag should error")
	}
	if _, err := ParseGworkspaceSetupArgs([]string{"-h"}); err != cli.ErrHelpRequested {
		t.Error("-h should return the cli.ErrHelpRequested sentinel")
	}
}

func TestGworkspaceSetup_SuccessDoesNotGuessOAuthAudience(t *testing.T) {
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
	opts := gworkspaceSetupOpts{Account: "you@example.com", Credentials: cred}
	if err := Setup(ge.env(), opts, strings.NewReader(""), &out, false, noCreds); err != nil {
		t.Fatalf("Setup: %v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "Testing") || strings.Contains(out.String(), "7 days") || strings.Contains(out.String(), "publish") {
		t.Errorf("setup must not guess whether the OAuth client is Internal or External, got:\n%s", out.String())
	}
}

// TestGworkspaceSetup_ZeroToolsFailsAndSkipsNotice: the headless probe
// returning a clean, empty tool list must fail the façade with the EXACT
// required sentence (never register/save anything).
func TestGworkspaceSetup_ZeroToolsFailsAndSkipsNotice(t *testing.T) {
	gogSetupTestCfg(t)
	cred := gogCredFile(t)
	ge := gogTestEnv{
		present: map[string]bool{"gog": true, "sbx": true},
		output: mergeOutputs(gogAuthCapabilityFixtures("setup", true), map[string]string{
			"gog auth --help": gogAuthHelpCurrentSetup,
			"gog --account you@example.com auth doctor --check": "ok",
			// clean exit, ZERO tools (empty output, no error).
			gogBareHeadlessKey("you@example.com"): "",
		}),
		statFile: map[string]bool{cred: true},
	}
	var out bytes.Buffer
	opts := gworkspaceSetupOpts{Account: "you@example.com", Credentials: cred}
	err := Setup(ge.env(), opts, strings.NewReader(""), &out, false, noCreds)
	if err == nil {
		t.Fatal("expected an error when the headless probe returns zero tools")
	}
	if !strings.Contains(err.Error(), "authorization succeeded but the headless tool listing returned 0 tools; nothing was registered") {
		t.Errorf("expected the exact required zero-tools sentence, got %q", err)
	}
}

// TestGworkspaceSetup_RollbackOnSaveFailure: registration succeeds but the
// subsequent config.Save() fails — the façade must surface the SAME rollback
// guarantee gog_setup.go provides (restoring the prior registration) and
func TestGworkspaceSetup_RollbackOnSaveFailure(t *testing.T) {
	env, cred, addCalls, rmCalls := gogR108RollbackEnv(t, true)
	var out bytes.Buffer
	opts := gworkspaceSetupOpts{Account: "you@example.com", Credentials: cred}
	err := Setup(env, opts, strings.NewReader(""), &out, false, noCreds)
	if err == nil {
		t.Fatal("expected an error when config.Save() fails after a successful registration")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("expected explicit rollback confirmation in the error, got %q", err)
	}
	if len(*rmCalls) != 0 || len(*addCalls) != 2 {
		t.Errorf("expected the prior registration to be RESTORED (2 add calls, 0 rm calls), got add=%v rm=%v", *addCalls, *rmCalls)
	}
}

// --- Status ------------------------------------------------------

// TestGworkspaceStatus_NotConfigured: no account, config.GWServerName not in the MCP
// set -> a single not-configured line and exit 0 (nothing to fail on since
// nothing was ever asked for).
func TestGworkspaceStatus_NotConfigured(t *testing.T) {
	cfg := &config.Config{}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", os.ErrNotExist }}}
	var out bytes.Buffer
	code := Status(cfg, env, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "not configured — set it up: pix gworkspace setup") {
		t.Errorf("expected the not-configured line, got:\n%s", out.String())
	}
}

// TestGworkspaceStatus_ConfiguredNoAccount: config.GWServerName is in the MCP set but
// no account is authorized -> a TODO pointing at `pix gworkspace setup`
// and a non-zero (needs-setup) exit code.
func TestGworkspaceStatus_ConfiguredNoAccount(t *testing.T) {
	cfg := &config.Config{MCP: []string{config.GWServerName}}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", os.ErrNotExist }}}
	var out bytes.Buffer
	code := Status(cfg, env, &out)
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (verified gap)", code)
	}
	if !strings.Contains(out.String(), "no account is authorized") || !strings.Contains(out.String(), "pix gworkspace setup") {
		t.Errorf("expected an unset-account TODO naming the setup command, got:\n%s", out.String())
	}
}

// TestGworkspaceStatus_ReadyPath: a fully registered, hardened, headless-proven
// account reports ready checks and exit 0 — plus the token-age line, which
// is UNVERIFIABLE by design (never a failure) and so must never affect the
// exit code on its own.
func TestGworkspaceStatus_ReadyPath(t *testing.T) {
	acct := "you@example.com"
	regCmd := bareGog(acct)
	probeKey := regCmd + " --list-tools"
	cfg := &config.Config{GogAccount: acct, MCP: []string{config.GWServerName}}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" || name == "gog" {
			return "/usr/bin/" + name, nil
		}
		return "", os.ErrNotExist
	}, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		switch key {
		case "sbx mcp get " + config.GWServerName:
			return "name: gog\ncommand: " + regCmd + "\n", nil
		case probeKey:
			return "gmail_search\ncalendar_events\n", nil
		}
		return "", os.ErrNotExist
	}}}
	var out bytes.Buffer
	code := Status(cfg, env, &out)
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (publication state is informational), output:\n%s", code, out.String())
	}
	s := out.String()
	for _, want := range []string{acct, "hardened read-only flags", "exposes tools"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in status output, got:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "✓ account") || !strings.Contains(s, "✓ read-only") || !strings.Contains(s, "✓ headless spawn") {
		t.Errorf("expected every real check (account/read-only/headless spawn) to be individually ready, got:\n%s", s)
	}
	if strings.Contains(s, "token age") || strings.Contains(s, "publication") {
		t.Errorf("status must report observed authorization health, not guess the OAuth audience, got:\n%s", s)
	}
}

// TestGworkspaceStatus_UnreadableRegistrationIsUnverifiableExitThree: sbx is
// absent (or exposes no command) -> the registration check is UNVERIFIABLE,
// and with nothing else confirmed broken the worst verdict (and exit code)
// is 3, never a false green (0) or a false hard failure (1).
func TestGworkspaceStatus_UnreadableRegistrationIsUnverifiableExitThree(t *testing.T) {
	cfg := &config.Config{GogAccount: "you@example.com", MCP: []string{config.GWServerName}}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", os.ErrNotExist }}}
	var out bytes.Buffer
	code := Status(cfg, env, &out)
	if code != 3 {
		t.Errorf("exit code = %d, want 3 (unverifiable, nothing confirmed broken)", code)
	}
	if !strings.Contains(out.String(), "could not read the "+config.GWServerName+" registration") {
		t.Errorf("expected the unreadable-registration note, got:\n%s", out.String())
	}
}

// --- Disable ------------------------------------------------------

// TestGworkspaceDisable_NotConfiguredIsCleanNoop: nothing set, nothing
// registered -> a no-op that touches neither config nor the gateway.
func TestGworkspaceDisable_NotConfiguredIsCleanNoop(t *testing.T) {
	cfg := &config.Config{}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == "sbx mcp ls" {
			return "", nil // confirmed absent
		}
		return "", os.ErrNotExist
	}}}
	var out bytes.Buffer
	if err := Disable(cfg, env, &out); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Errorf("expected a clean no-op message, got:\n%s", out.String())
	}
}

// TestGworkspaceDisable_UnknownRegistrationRefuses: `sbx mcp ls` fails, so the
// registration snapshot is gogRegUnknown — disable must refuse rather than
// guess, and must never silently drop config while the gateway state is
// unreadable.
func TestGworkspaceDisable_UnknownRegistrationRefuses(t *testing.T) {
	cfg := &config.Config{GogAccount: "you@example.com", MCP: []string{config.GWServerName}}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		return "", os.ErrNotExist // every `sbx mcp ...` call fails
	}}}
	var out bytes.Buffer
	err := Disable(cfg, env, &out)
	if err == nil {
		t.Fatal("expected a refusal when the registration state cannot be confirmed")
	}
	if !strings.Contains(err.Error(), "could not confirm") {
		t.Errorf("expected the could-not-confirm refusal, got %q", err)
	}
	if cfg.GogAccount == "" {
		t.Error("a refused disable must never clear config as a side effect")
	}
}

// TestGworkspaceDisable_PresentRemovesRegistrationAndConfig: a confirmed
// registration is removed from the gateway FIRST, then config is cleared —
// the ordering documented on Disable (a save failure after a
// successful unregister must never leave a persisted "still registered"
// config the gateway no longer honors).
func TestGworkspaceDisable_PresentRemovesRegistrationAndConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.SetGogAccount("you@example.com")
	cfg.AddMCP(config.GWServerName)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	var rmCalls [][]string
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if key == "sbx mcp ls" {
			return config.GWServerName + "\n", nil
		}
		if key == "sbx mcp get "+config.GWServerName {
			return "name: gog\ncommand: " + bareGog("you@example.com") + "\n", nil
		}
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "rm" {
			rmCalls = append(rmCalls, append([]string{}, args...))
			return "", nil
		}
		return "", os.ErrNotExist
	}}}
	var out bytes.Buffer
	if err := Disable(cfg, env, &out); err != nil {
		t.Fatalf("Disable: %v\n%s", err, out.String())
	}
	if len(rmCalls) != 1 || rmCalls[0][2] != config.GWServerName {
		t.Fatalf("expected a `sbx mcp rm %s` call, got %v", config.GWServerName, rmCalls)
	}
	if cfg.GogAccount != "" || slices.Contains(cfg.MCP, config.GWServerName) {
		t.Errorf("config must be cleared after disable, got account=%q mcp=%v", cfg.GogAccount, cfg.MCP)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.GogAccount != "" || slices.Contains(reloaded.MCP, config.GWServerName) {
		t.Errorf("disable must persist the cleared config, got account=%q mcp=%v", reloaded.GogAccount, reloaded.MCP)
	}
	if !strings.Contains(out.String(), "off") || !strings.Contains(out.String(), gwPermissionsURL) {
		t.Errorf("expected the off + revoke-yourself message naming the permissions URL, got:\n%s", out.String())
	}
}

// --- flag-gating: --account/--credentials require the opt-in --------------

func TestGworkspaceUsage_NamingContract(t *testing.T) {
	blocks := map[string]string{
		"Usage":        Usage,
		"SetupUsage":   SetupUsage,
		"StatusUsage":  StatusUsage,
		"DisableUsage": DisableUsage,
	}
	// Lines legitimately allowed to name the dependency binary (the ONE
	// exception the header documents).
	allowed := []string{config.GWInstallCmd, gwUpgradeCmd}
	for name, block := range blocks {
		if !strings.Contains(block, "gworkspace") {
			t.Errorf("%s must use the public CLI noun `gworkspace`, got:\n%s", name, block)
		}
		for _, line := range strings.Split(block, "\n") {
			if !strings.Contains(line, "gog") {
				continue
			}
			ok := false
			for _, a := range allowed {
				if strings.Contains(line, a) {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s: line %q names the dependency binary outside the allowed install/upgrade lines", name, line)
			}
		}
	}
	if config.GWServerName != "google-workspace" {
		t.Errorf("config.GWServerName = %q, want the public MCP display name", config.GWServerName)
	}
}

// TestGworkspaceDisable_NeverPrintsLegacyGogVerb: disable's own output must
// never reference a `pix gog ...` command — that verb tree is DELETED,
// not renamed (6b39a69), so any surviving reference would send a user to a
// dead command.
func TestGworkspaceDisable_NeverPrintsLegacyGogVerb(t *testing.T) {
	cfg := &config.Config{}
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if strings.Join(append([]string{name}, args...), " ") == "sbx mcp ls" {
			return "", nil
		}
		return "", os.ErrNotExist
	}}}
	var out bytes.Buffer
	if err := Disable(cfg, env, &out); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if strings.Contains(out.String(), "pix gog ") {
		t.Errorf("disable output references the deleted `pix gog` verb tree, got:\n%s", out.String())
	}
}
