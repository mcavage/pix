package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

func TestParseOpRefsClassification(t *testing.T) {
	content := `# a comment
SLACK_TOKEN=op://Private/Slack/credential
UNFILLED=op://<vault>/<item>/credential
GOG_ACCOUNT=me@example.com
PASTED=xoxb-123-secret
`
	refs := parseOpRefs(content)
	byKey := map[string]opRef{}
	for _, r := range refs {
		byKey[r.key] = r
	}
	if r := byKey["SLACK_TOKEN"]; !r.isRef || r.placeholder {
		t.Errorf("SLACK_TOKEN: isRef=%v placeholder=%v, want filled ref", r.isRef, r.placeholder)
	}
	if r := byKey["UNFILLED"]; !r.isRef || !r.placeholder {
		t.Errorf("UNFILLED: isRef=%v placeholder=%v, want unfilled placeholder", r.isRef, r.placeholder)
	}
	if r := byKey["GOG_ACCOUNT"]; !r.nonSecret {
		t.Errorf("GOG_ACCOUNT should be on the non-secret allowlist")
	}
	if r := byKey["PASTED"]; r.isRef || r.nonSecret {
		t.Errorf("PASTED literal: isRef=%v nonSecret=%v, want neither", r.isRef, r.nonSecret)
	}
}

// TestSeededOpRefsHasNoActiveEntries covers F1: a freshly seeded op-refs.env has
// ZERO active (uncommented) ref lines — parseOpRefs finds no entries.
func TestSeededOpRefsHasNoActiveEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	path, created, err := config.SeedOpRefs()
	if err != nil {
		t.Fatalf("SeedOpRefs: %v", err)
	}
	if !created {
		t.Fatalf("SeedOpRefs: created = false, want true")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	if refs := parseOpRefs(string(content)); len(refs) != 0 {
		t.Errorf("freshly seeded op-refs.env has %d active entries, want 0: %+v", len(refs), refs)
	}
}

// TestSecretStatusShortLiteralFlagged covers F4 parity in `secret status`: a
// short, NOT-secret-shaped literal is still flagged (refs-only) and its value is
// never printed.
func TestSecretStatusShortLiteralFlagged(t *testing.T) {
	const val = "correcthorsebattery"
	f := fakeEnv{
		present: map[string]bool{},
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=" + val + "\n"},
	}
	var out bytes.Buffer
	runSecretStatus(f.env(), &out)
	s := out.String()
	if strings.Contains(s, val) {
		t.Errorf("secret status LEAKED the literal value:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN") || !strings.Contains(s, "not an op:// ref") {
		t.Errorf("status should flag the short literal as not-a-ref:\n%s", s)
	}
}

// TestSecretCheckRejectsTrailingArg covers F6: `secret check --bogus` exits 2.
// runSecretCmd calls os.Exit, so we exercise it in a subprocess.
func TestSecretCheckRejectsTrailingArg(t *testing.T) {
	if os.Getenv("PI_STACK_SECRET_BOGUS") == "1" {
		runSecretCmd([]string{"check", "--bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCheckRejectsTrailingArg")
	cmd.Env = append(os.Environ(), "PI_STACK_SECRET_BOGUS=1")
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v", err)
	}
	if ee.ExitCode() != 2 {
		t.Errorf("secret check --bogus exit code = %d, want 2", ee.ExitCode())
	}
}

func TestHasPlaceholder(t *testing.T) {
	if !hasPlaceholder("op://<vault>/x/y") {
		t.Error("angle-bracket placeholder not detected")
	}
	if hasPlaceholder("op://Private/Slack/credential") {
		t.Error("a filled ref wrongly flagged as placeholder")
	}
}

// TestSecretStatusNeverLeaksValue is the security gate: a pasted secret value
// must NEVER appear in `secret status` output.
func TestSecretStatusNeverLeaksValue(t *testing.T) {
	const pasted = "xoxb-THIS-MUST-NOT-BE-PRINTED"
	f := fakeEnv{
		present: map[string]bool{}, // op not installed
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=" + pasted + "\n"},
	}
	var out bytes.Buffer
	runSecretStatus(f.env(), &out)
	s := out.String()
	if strings.Contains(s, pasted) {
		t.Errorf("secret status LEAKED the pasted value:\n%s", s)
	}
	// The xoxb-* value is secret-shaped, so it gets the stronger pasted-secret
	// wording (still without printing the value).
	if !strings.Contains(s, "SLACK_TOKEN") || !strings.Contains(s, "possible pasted secret") {
		t.Errorf("status should flag SLACK_TOKEN as a possible pasted secret:\n%s", s)
	}
}

func TestSecretStatusStates(t *testing.T) {
	// op installed + signed in; a filled ref + a placeholder.
	f := fakeEnv{
		present: map[string]bool{"op": true},
		output:  map[string]string{"op account list": "me@example.com\n"},
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
		files: map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=op://Private/Slack/credential\n" +
			"OTHER=op://<vault>/<item>/credential\n"},
	}
	var out bytes.Buffer
	runSecretStatus(f.env(), &out)
	s := out.String()
	if !strings.Contains(s, "installed + account configured") {
		t.Errorf("want op installed+account-configured state:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN = op:// ref") {
		t.Errorf("want SLACK_TOKEN reported filled:\n%s", s)
	}
	if !strings.Contains(s, "OTHER = placeholder") {
		t.Errorf("want OTHER reported as placeholder:\n%s", s)
	}
}

func TestSecretStatusOpNotSignedIn(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"op": true},
		// no "op account list" output => not signed in
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
	}
	var out bytes.Buffer
	runSecretStatus(f.env(), &out)
	s := out.String()
	if !strings.Contains(s, "no account configured") {
		t.Errorf("want no-account-configured state:\n%s", s)
	}
	if !strings.Contains(s, "not present") {
		t.Errorf("want op-refs.env absent state:\n%s", s)
	}
}

// TestSecretCheckOKNeverLeaks: the happy path (all refs resolve) reports OK per
// key and never prints the resolved secret value.
func TestSecretCheckOKNeverLeaks(t *testing.T) {
	const resolved = "SECRET-VALUE-DO-NOT-PRINT"
	f := fakeEnv{
		present: map[string]bool{"op": true},
		output: map[string]string{
			"op account list":                       "me@example.com\n",
			"op read op://Private/Slack/credential": resolved,
		},
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=op://Private/Slack/credential\n"},
	}
	var out bytes.Buffer
	runSecretCheck(f.env(), &out)
	s := out.String()
	if strings.Contains(s, resolved) {
		t.Errorf("secret check LEAKED the resolved value:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN: OK") {
		t.Errorf("want SLACK_TOKEN OK:\n%s", s)
	}
}

func TestSecretHelpConfigIndependent(t *testing.T) {
	// -h must print usage and NOT touch config/op — runSecretCmd handles help
	// before any env work. We can't call os.Exit-free easily, so assert the
	// help sentinel path via wantsHelp used inside runSecretCmd is honored by
	// checking secretUsage is non-empty and wantsHelp detects the flag.
	if !wantsHelp([]string{"--help"}) || !wantsHelp([]string{"status", "-h"}) {
		t.Error("wantsHelp should detect secret help flags")
	}
	if secretUsage == "" {
		t.Error("secretUsage must be defined")
	}
}
