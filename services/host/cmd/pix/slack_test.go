// slack_test.go covers the `pix slack setup|status|disable` CLI (slack.go):
// argument parsing, the pasted-token/non-xoxp- rejections (and that neither
// ever leaks into output), that an auth.test failure writes and registers
// nothing, status's identity-pin mismatch + registered-vs-attachment
// wording, and disable's revocation warning.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/config"
)

// --- parseSlackSetupArgs -----------------------------------------------

func TestParseSlackSetupArgs(t *testing.T) {
	o, err := parseSlackSetupArgs([]string{"--token-ref", "op://Private/Slack/credential", "--yes"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.tokenRef != "op://Private/Slack/credential" || !o.assumeYes {
		t.Errorf("parsed = %+v", o)
	}

	o2, err := parseSlackSetupArgs([]string{"--token-ref=op://Private/Slack/credential", "-y"})
	if err != nil {
		t.Fatalf("parse (= form, -y): %v", err)
	}
	if o2.tokenRef != "op://Private/Slack/credential" || !o2.assumeYes {
		t.Errorf("parsed (= form) = %+v", o2)
	}

	if _, err := parseSlackSetupArgs([]string{"--token-ref"}); err == nil {
		t.Error("--token-ref without a value should error")
	}
	if _, err := parseSlackSetupArgs([]string{"--bogus"}); err == nil {
		t.Error("an unknown flag should error")
	}
	pasted := "xoxp-should-never-be-echoed"
	if _, err := parseSlackSetupArgs([]string{pasted}); err == nil || strings.Contains(err.Error(), pasted) || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("a positional pasted token must be rejected and redacted, got %v", err)
	}
	if _, err := parseSlackSetupArgs([]string{"-h"}); err != errHelpRequested {
		t.Error("-h should return the errHelpRequested sentinel")
	}
	if _, err := parseSlackSetupArgs([]string{"--token-ref", "op://x/y/z", "--help"}); err != errHelpRequested {
		t.Error("--help should return the errHelpRequested sentinel even after other flags")
	}
}

// TestParseSlackSetupArgsPKCEFlags proves --client-id/--redirect-uri/--vault
// parse in both "space" and "=" forms, and that --vault defaults to "Private"
// when omitted (vaultExplicit false) but is tracked as explicit when passed.
func TestParseSlackSetupArgsPKCEFlags(t *testing.T) {
	o, err := parseSlackSetupArgs(nil)
	if err != nil {
		t.Fatalf("parse empty argv: %v", err)
	}
	if o.vault != "Private" || o.vaultExplicit {
		t.Errorf("default vault = %q explicit=%v, want Private/false", o.vault, o.vaultExplicit)
	}

	o2, err := parseSlackSetupArgs([]string{"--client-id", "C123", "--redirect-uri", "http://localhost:9/slack/callback", "--vault", "Shared"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o2.clientID != "C123" || o2.redirectURI != "http://localhost:9/slack/callback" || o2.vault != "Shared" || !o2.vaultExplicit {
		t.Errorf("parsed = %+v", o2)
	}

	o3, err := parseSlackSetupArgs([]string{"--client-id=C456", "--redirect-uri=http://localhost:9/slack/callback", "--vault=Shared2"})
	if err != nil {
		t.Fatalf("parse (= form): %v", err)
	}
	if o3.clientID != "C456" || o3.redirectURI != "http://localhost:9/slack/callback" || o3.vault != "Shared2" || !o3.vaultExplicit {
		t.Errorf("parsed (= form) = %+v", o3)
	}

	if _, err := parseSlackSetupArgs([]string{"--client-id"}); err == nil {
		t.Error("--client-id without a value should error")
	}
	if _, err := parseSlackSetupArgs([]string{"--redirect-uri"}); err == nil {
		t.Error("--redirect-uri without a value should error")
	}
	if _, err := parseSlackSetupArgs([]string{"--vault"}); err == nil {
		t.Error("--vault without a value should error")
	}
}

// TestParseSlackSetupArgsRejectsMixedStaticAndPKCEFlags proves --token-ref
// combined with any PKCE-only flag is refused up front, rather than silently
// picking one path — a user who passes both almost certainly meant only one.
func TestParseSlackSetupArgsRejectsMixedStaticAndPKCEFlags(t *testing.T) {
	cases := [][]string{
		{"--token-ref", "op://v/i/f", "--client-id", "C123"},
		{"--token-ref", "op://v/i/f", "--redirect-uri", "http://localhost:9/slack/callback"},
		{"--token-ref", "op://v/i/f", "--vault", "Shared"},
		{"--token-ref", "op://v/i/f", "--allow-identity-change"},
	}
	for _, argv := range cases {
		if _, err := parseSlackSetupArgs(argv); err == nil {
			t.Errorf("argv %v: expected a mixed-flags refusal, got success", argv)
		}
	}
	// A PKCE-only combination (no --token-ref) must still parse fine.
	if _, err := parseSlackSetupArgs([]string{"--client-id", "C123", "--allow-identity-change"}); err != nil {
		t.Errorf("PKCE-only flags should parse cleanly, got %v", err)
	}
}

// --- slackSetup hermetic harness ----------------------------------------

// slackTestEnv builds a shellEnv over a real (but temp-dir-scoped) config +
// op-refs.env — config.Load/Save and defaultOpRefsPath both honor PIX_CONFIG,
// so using defaultShellEnv() here means the disk-touching bits behave exactly
// as production, while run/lookPath/probe/slackAuthTest are faked so nothing
// ever shells out or hits the network.
type slackTestEnv struct {
	calls      [][]string
	opToken    string
	opOK       bool
	sbxPresent bool
	sbxAddErr  error
	authTest   func(token string) (slackIdentity, error)
}

func (f *slackTestEnv) env() shellEnv {
	e := defaultShellEnv()
	e.probe = nil // force probeRun through env.run (faked below), never a real exec
	e.lookPath = func(name string) (string, error) {
		switch name {
		case "sbx":
			if f.sbxPresent {
				return "/fake/bin/sbx", nil
			}
			return "", fmt.Errorf("not found")
		case "op":
			return "/fake/bin/op", nil
		}
		return "", fmt.Errorf("not found")
	}
	e.run = func(name string, args ...string) (string, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		switch name {
		case "op":
			if len(args) >= 1 && args[0] == "read" {
				if !f.opOK {
					return "", fmt.Errorf("op read failed")
				}
				return f.opToken, nil
			}
			return "", nil
		case "sbx":
			if len(args) >= 2 && args[0] == "mcp" && args[1] == "rm" {
				return "", nil // rollback/disable removal always "succeeds" in these tests
			}
			return "", f.sbxAddErr
		case "fake-pix-host":
			if len(args) >= 2 && args[0] == "mcp" && args[1] == "--list" {
				return "slack\n", nil
			}
			return "", nil
		}
		return "", nil
	}
	e.slackAuthTest = f.authTest
	return e
}

func fakeHostResolver() (string, error) { return "fake-pix-host", nil }

// slackTestCfg points config.Load()/Save() and (via the real getenv/statFile
// wired by defaultShellEnv) op-refs.env at a fresh temp dir for one test.
func slackTestCfg(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	return dir
}

func (f *slackTestEnv) ranSbxAdd() bool {
	for _, c := range f.calls {
		if len(c) >= 3 && c[0] == "sbx" && c[1] == "mcp" && c[2] == "add" {
			return true
		}
	}
	return false
}

func opRefsFileContent(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(config.OpRefsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatalf("read op-refs.env: %v", err)
	}
	return string(b)
}

// --- reject pasted / non-xoxp- --------------------------------------------

func TestSlackSetupRejectsPastedToken(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{}
	var out bytes.Buffer
	// Construct at runtime so secret scanners do not mistake the deliberately
	// fake fixture for a live Slack token embedded in repository history.
	pasted := "xoxp-" + strings.Repeat("not-a-real-token-", 4)
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: pasted, assumeYes: true}, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil {
		t.Fatal("a pasted (non-op://) --token-ref must be rejected")
	}
	if strings.Contains(err.Error(), pasted) || strings.Contains(out.String(), pasted) {
		t.Errorf("the pasted token must never be echoed back; err=%q out=%q", err, out.String())
	}
	if len(f.calls) != 0 {
		t.Errorf("a rejected --token-ref must never touch op/sbx: calls = %v", f.calls)
	}
	if got := opRefsFileContent(t); got != "" {
		t.Errorf("op-refs.env must stay untouched, got:\n%s", got)
	}
}

func TestSlackSetupRefusesForeignExistingRegistration(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{opOK: true, opToken: "xoxp-unused", sbxPresent: true}
	e := f.env()
	e.run = func(name string, args ...string) (string, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "slack\n", nil
		}
		if name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "get" {
			return "name: slack\ncommand: /tmp/not-pix-host mcp slack\n", nil
		}
		return "", nil
	}
	var out bytes.Buffer
	err := slackSetup(e, slackSetupOpts{tokenRef: "op://Private/Slack/credential", assumeYes: true},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("foreign registration should be refused, got %v", err)
	}
	for _, c := range f.calls {
		if len(c) > 1 && c[0] == "op" && c[1] == "read" {
			t.Fatalf("foreign registration must be rejected before token resolution, calls=%v", f.calls)
		}
	}
}

func TestSlackSetupRejectsNonXoxpToken(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{opOK: true, opToken: "xoxb-not-a-user-token", sbxPresent: true}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential", assumeYes: true},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil {
		t.Fatal("a resolved token without the xoxp- prefix must be rejected")
	}
	if strings.Contains(err.Error(), f.opToken) || strings.Contains(out.String(), f.opToken) {
		t.Errorf("the resolved token value must never be echoed back; err=%q out=%q", err, out.String())
	}
	if f.ranSbxAdd() {
		t.Error("a rejected token must never be registered")
	}
	if got := opRefsFileContent(t); got != "" {
		t.Errorf("op-refs.env must stay untouched, got:\n%s", got)
	}
}

// --- auth failure: no write, no register ----------------------------------

func TestSlackSetupAuthTestFailureNoWriteNoRegister(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) { return slackIdentity{}, fmt.Errorf("invalid_auth") },
	}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential", assumeYes: true},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil {
		t.Fatal("a failed live auth.test must fail the command")
	}
	if strings.Contains(err.Error(), f.opToken) || strings.Contains(out.String(), f.opToken) {
		t.Errorf("the token must never be echoed on an auth.test failure; err=%q out=%q", err, out.String())
	}
	if f.ranSbxAdd() {
		t.Error("an auth.test failure must never register anything")
	}
	if got := opRefsFileContent(t); got != "" {
		t.Errorf("an auth.test failure must never write op-refs.env, got:\n%s", got)
	}
	cfg, _ := config.Load()
	if mcpConfigured(cfg, slackServerName) {
		t.Error("an auth.test failure must never add slack to the configured mcp set")
	}
}

func TestSlackSetupRejectsIdentityWithoutStableIDs(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", user: "jane"}, nil
		},
	}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential", assumeYes: true},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "team_id or user_id") {
		t.Fatalf("missing stable identity ids should fail, got %v", err)
	}
	if got := opRefsFileContent(t); got != "" {
		t.Errorf("missing identity ids must not write refs, got:\n%s", got)
	}
}

func TestSlackSetupDeclinedConfirmationWritesNothing(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", teamID: "T1", user: "jane", userID: "U1"}, nil
		},
	}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential"},
		strings.NewReader("n\n"), &out, true, fakeHostResolver)
	if err != nil {
		t.Fatalf("declining confirmation should abort cleanly, not error: %v", err)
	}
	if f.ranSbxAdd() {
		t.Error("a declined confirmation must never register anything")
	}
	if got := opRefsFileContent(t); got != "" {
		t.Errorf("a declined confirmation must never write op-refs.env, got:\n%s", got)
	}
}

func TestSlackSetupNonInteractiveWithoutYesFails(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", teamID: "T1", user: "jane", userID: "U1"}, nil
		},
	}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential"},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil {
		t.Fatal("non-interactive without --yes must refuse rather than silently proceed")
	}
	if f.ranSbxAdd() {
		t.Error("a refused non-interactive run must never register anything")
	}
}

// --- happy path: writes refs, registers, saves config ---------------------

func TestSlackSetupHappyPath(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", teamID: "T123", user: "jane", userID: "U456"}, nil
		},
	}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential", assumeYes: true},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err != nil {
		t.Fatalf("slackSetup: %v\n--- output ---\n%s", err, out.String())
	}
	if strings.Contains(out.String(), f.opToken) {
		t.Errorf("the token must never be printed, got:\n%s", out.String())
	}
	content := opRefsFileContent(t)
	if !strings.Contains(content, "SLACK_TOKEN=op://Private/Slack/credential") {
		t.Errorf("op-refs.env must carry the SLACK_TOKEN ref, got:\n%s", content)
	}
	if !strings.Contains(content, "SLACK_TEAM_ID=T123") || !strings.Contains(content, "SLACK_USER_ID=U456") {
		t.Errorf("op-refs.env must carry the identity pins, got:\n%s", content)
	}
	if !f.ranSbxAdd() {
		t.Errorf("slack must be registered with the sbx gateway, calls = %v", f.calls)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !mcpConfigured(cfg, slackServerName) {
		t.Error("slack must be added to the configured mcp set")
	}
}

// TestSlackSetupStaticRefusesWhenOAuthConfigured proves --token-ref refuses
// outright once a COMPLETE [slack] OAuth wiring already exists — writing
// SLACK_TOKEN would be silently shadowed by OAuth (slackDefaultTokenSource in
// services/host/slack.go always prefers OAuth), so the static path must
// refuse and point at `pix slack disable` rather than let that trap exist.
// Nothing should be written.
func TestSlackSetupStaticRefusesWhenOAuthConfigured(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	_ = cfg

	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", teamID: "T123", user: "jane", userID: "U456"}, nil
		},
	}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential", assumeYes: true},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "pix slack disable") {
		t.Fatalf("expected a refusal naming `pix slack disable`, got %v", err)
	}
	if got := opRefsFileContent(t); strings.Contains(got, "SLACK_TOKEN=op://Private/Slack/credential") {
		t.Errorf("nothing should be written when refusing, got:\n%s", got)
	}
	if f.ranSbxAdd() {
		t.Error("slack must not be registered when the static setup refuses")
	}
}

func TestSlackSetupHardFailsWhenSbxAbsent(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: false,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", teamID: "T123", user: "jane", userID: "U456"}, nil
		},
	}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{tokenRef: "op://Private/Slack/credential", assumeYes: true},
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil {
		t.Fatal("setup must hard-fail (non-zero) when sbx is absent, not silently succeed")
	}
	cfg, _ := config.Load()
	if mcpConfigured(cfg, slackServerName) {
		t.Error("config must not claim slack is configured when registration never happened")
	}
	if got := opRefsFileContent(t); got != "" {
		t.Errorf("sbx preflight failure must happen before refs are written, got:\n%s", got)
	}
}

// --- status: identity pin mismatch + registered-vs-attachment wording -----

func opRefsWith(t *testing.T, lines ...string) {
	t.Helper()
	if _, _, err := config.SeedOpRefs(); err != nil {
		t.Fatalf("seed op-refs.env: %v", err)
	}
	content := config.OpRefsTemplate + "\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(config.OpRefsPath(), []byte(content), 0o600); err != nil {
		t.Fatalf("write op-refs.env: %v", err)
	}
}

func TestSlackStatusIdentityPinMismatch(t *testing.T) {
	slackTestCfg(t)
	opRefsWith(t,
		"SLACK_TOKEN=op://Private/Slack/credential",
		"SLACK_TEAM_ID=T_OLD",
		"SLACK_USER_ID=U_OLD",
	)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-rotated-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", teamID: "T_NEW", user: "jane", userID: "U_NEW"}, nil
		},
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	var out bytes.Buffer
	exit := slackStatus(cfg, f.env(), &out, time.Now())
	if exit != 1 {
		t.Errorf("exit = %d, want 1 (a verified mismatch)", exit)
	}
	if !strings.Contains(out.String(), "identity pin") || !strings.Contains(out.String(), "does not match") {
		t.Errorf("status must call out the identity pin mismatch, got:\n%s", out.String())
	}
}

func TestSlackStatusIdentityPinMatch(t *testing.T) {
	slackTestCfg(t)
	opRefsWith(t,
		"SLACK_TOKEN=op://Private/Slack/credential",
		"SLACK_TEAM_ID=T123",
		"SLACK_USER_ID=U456",
	)
	f := &slackTestEnv{
		opOK: true, opToken: "xoxp-real-looking-token", sbxPresent: true,
		authTest: func(string) (slackIdentity, error) {
			return slackIdentity{team: "Acme", teamID: "T123", user: "jane", userID: "U456"}, nil
		},
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	var out bytes.Buffer
	// registration is still not-registered in this fixture (no `sbx mcp get`
	// stub wired), so the OVERALL exit still reflects that separate gap; this
	// test only asserts the identity pin line itself reads as matched.
	slackStatus(cfg, f.env(), &out, time.Now())
	if !strings.Contains(out.String(), "matches the identity pinned at setup") {
		t.Errorf("status must report a matching pin as matched, got:\n%s", out.String())
	}
}

// TestSlackStatusRegisteredVsAttachmentWording proves status reports
// registration and attachment as two DISTINCT lines, never collapsed into one
// boolean (docs/design/slack-setup.md's "registration is not the same as
// attachment" invariant).
func TestSlackStatusRejectsForeignRegistration(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{sbxPresent: true}
	e := f.env()
	e.run = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "get" && args[2] == "slack" {
			return "name: slack\ncommand: /tmp/not-pix-host mcp slack\n", nil
		}
		return "", nil
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	var out bytes.Buffer
	if exit := slackStatus(cfg, e, &out, time.Now()); exit != 1 {
		t.Errorf("foreign registration exit = %d, want 1", exit)
	}
	if !strings.Contains(out.String(), "not the canonical Pix host command") {
		t.Errorf("foreign registration must not be reported ready:\n%s", out.String())
	}
}

func TestSlackStatusRegisteredVsAttachmentWording(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{sbxPresent: true}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	var out bytes.Buffer
	slackStatus(cfg, f.env(), &out, time.Now())
	text := out.String()
	if !strings.Contains(text, "registration") {
		t.Errorf("status must have a distinct 'registration' line, got:\n%s", text)
	}
	if !strings.Contains(text, "attachment") {
		t.Errorf("status must have a distinct 'attachment' line, got:\n%s", text)
	}
	if !strings.Contains(text, "NOT the same as attachment") {
		t.Errorf("status must explain registration != attachment, got:\n%s", text)
	}
}

// --- disable: removes refs + prints the revocation warning ----------------

func TestSlackDisableRemovesRefsAndWarnsAboutRevocation(t *testing.T) {
	slackTestCfg(t)
	opRefsWith(t,
		"SLACK_TOKEN=op://Private/Slack/credential",
		"SLACK_TEAM_ID=T123",
		"SLACK_USER_ID=U456",
	)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.AddMCP(slackServerName)
	if err := cfg.Save(); err != nil {
		t.Fatalf("config.Save: %v", err)
	}

	f := &slackTestEnv{sbxPresent: true}
	e := f.env()
	e.hostBinary = func() (string, error) { return "/fake/bin/pix-host", nil }
	// The tri-state probe reports slack present, and definition inspection
	// proves it is the canonical Pix host command before disable removes it.
	e.run = func(name string, args ...string) (string, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "slack\n", nil
		}
		if name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "get" {
			return "name: slack\ncommand: /fake/bin/pix-host mcp slack\n", nil
		}
		return "", nil
	}

	var out bytes.Buffer
	if err := slackDisable(cfg, e, &out); err != nil {
		t.Fatalf("slackDisable: %v\n--- output ---\n%s", err, out.String())
	}

	text := out.String()
	if !strings.Contains(text, "does NOT revoke the token at Slack") {
		t.Errorf("disable must explicitly warn that revocation is still required, got:\n%s", text)
	}
	if !strings.Contains(text, "may keep using it until restarted") {
		t.Errorf("disable must warn a running process may retain the token, got:\n%s", text)
	}

	removedRegistration := false
	for _, c := range f.calls {
		if len(c) >= 3 && c[0] == "sbx" && c[1] == "mcp" && c[2] == "rm" {
			removedRegistration = true
		}
	}
	if !removedRegistration {
		t.Errorf("disable must remove the sbx registration, calls = %v", f.calls)
	}

	// Parse (not substring-match) so the template's own COMMENTED example line
	// ("# SLACK_TOKEN=op://<vault>/<item>/<field>") is never mistaken for a
	// surviving active entry.
	remaining := map[string]bool{}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		remaining[r.key] = true
	}
	for _, key := range []string{"SLACK_TOKEN", "SLACK_TEAM_ID", "SLACK_USER_ID"} {
		if remaining[key] {
			t.Errorf("disable must remove %s from op-refs.env, got:\n%s", key, opRefsFileContent(t))
		}
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load after disable: %v", err)
	}
	if mcpConfigured(got, slackServerName) {
		t.Error("disable must remove slack from the configured mcp set")
	}
}

func TestSlackDisableRemovesOrphanIdentityPins(t *testing.T) {
	slackTestCfg(t)
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	f := &slackTestEnv{sbxPresent: true}
	e := f.env()
	e.run = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "", nil
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := slackDisable(cfg, e, &out); err != nil {
		t.Fatalf("slackDisable with orphan pins: %v", err)
	}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		if r.key == "SLACK_TEAM_ID" || r.key == "SLACK_USER_ID" {
			t.Fatalf("orphan pin survived disable: %s", r.key)
		}
	}
}

func TestSlackDisableRefusesForeignRegistration(t *testing.T) {
	slackTestCfg(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	f := &slackTestEnv{sbxPresent: true}
	e := f.env()
	e.run = func(name string, args ...string) (string, error) {
		f.calls = append(f.calls, append([]string{name}, args...))
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "slack\n", nil
		}
		if name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "get" {
			return "name: slack\ncommand: /tmp/not-pix-host mcp slack\n", nil
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := slackDisable(cfg, e, &out); err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("foreign registration should be preserved, got %v", err)
	}
	for _, c := range f.calls {
		if len(c) >= 3 && c[0] == "sbx" && c[1] == "mcp" && c[2] == "rm" {
			t.Fatalf("foreign registration was removed: calls=%v", f.calls)
		}
	}
}

func TestSlackDisableNoopWhenNothingConfigured(t *testing.T) {
	slackTestCfg(t)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	f := &slackTestEnv{sbxPresent: true}
	e := f.env()
	e.run = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "", nil // nothing registered
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := slackDisable(cfg, e, &out); err != nil {
		t.Fatalf("slackDisable on a clean config should be a no-op, got: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Errorf("expected a clean no-op message, got:\n%s", out.String())
	}
}

// --- verb/help coverage ---------------------------------------------------

// TestSlackVerbDiscoverable is a focused sibling of the generic
// TestHelpListsEveryTopLevelVerb/TestManPageDocumentsEveryKnownVerb checks
// (verbcoverage_test.go, man_test.go), which already cover `slack` because
// they read the dispatch switch / knownVerbs live. This pins the three
// subcommands specifically, so a future edit that drops one from slackUsage
// fails locally in this file too.
func TestSlackVerbDiscoverable(t *testing.T) {
	if !knownVerbs["slack"] {
		t.Error(`"slack" must be in knownVerbs so it is discoverable/documented`)
	}
	usage, ok := verbUsage("slack")
	if !ok {
		t.Fatal(`verbUsage("slack") must resolve`)
	}
	for _, sub := range []string{"setup", "status", "disable"} {
		if !strings.Contains(usage, sub) {
			t.Errorf("pix slack usage is missing subcommand %q:\n%s", sub, usage)
		}
	}
}
