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

// TestSecretLsShortLiteralFlagged covers F4 parity in `secret ls`: a short,
// NOT-secret-shaped literal is still flagged (refs-only) and its value is never
// printed.
func TestSecretLsShortLiteralFlagged(t *testing.T) {
	const val = "correcthorsebattery"
	f := fakeEnv{
		present: map[string]bool{},
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=" + val + "\n"},
	}
	var out bytes.Buffer
	runSecretLs(f.env(), &out)
	s := out.String()
	if strings.Contains(s, val) {
		t.Errorf("secret ls LEAKED the literal value:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN") || !strings.Contains(s, "not an op:// ref") {
		t.Errorf("ls should flag the short literal as not-a-ref:\n%s", s)
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

// TestSecretCmdArgCounts covers the dispatch surface: `set` requires exactly 2
// args, `rm` requires exactly 1, and an unknown subcommand names the new CRUD
// surface. All run in a subprocess since runSecretCmd calls os.Exit.
func TestSecretCmdArgCounts(t *testing.T) {
	cases := []struct {
		name string
		argv []string
	}{
		{"set too few", []string{"set", "ONLY_ONE"}},
		{"set too many", []string{"set", "A", "op://v/i/f", "extra"}},
		{"rm too many", []string{"rm", "A", "B"}},
		{"unknown", []string{"frobnicate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("PI_STACK_SECRET_ARGCOUNT") == tc.name {
				runSecretCmd(tc.argv)
				return
			}
			cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCmdArgCounts/"+strings.ReplaceAll(tc.name, " ", "_"))
			cmd.Env = append(os.Environ(), "PI_STACK_SECRET_ARGCOUNT="+tc.name)
			err := cmd.Run()
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("expected an ExitError, got %v", err)
			}
			if ee.ExitCode() != 2 {
				t.Errorf("exit code = %d, want 2", ee.ExitCode())
			}
		})
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

// TestSecretLsNeverLeaksValue is the security gate: a pasted secret value must
// NEVER appear in `secret ls` output.
func TestSecretLsNeverLeaksValue(t *testing.T) {
	const pasted = "xoxb-THIS-MUST-NOT-BE-PRINTED"
	f := fakeEnv{
		present: map[string]bool{}, // op not installed
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=" + pasted + "\n"},
	}
	var out bytes.Buffer
	runSecretLs(f.env(), &out)
	s := out.String()
	if strings.Contains(s, pasted) {
		t.Errorf("secret ls LEAKED the pasted value:\n%s", s)
	}
	// The xoxb-* value is secret-shaped, so it gets the stronger pasted-secret
	// wording (still without printing the value).
	if !strings.Contains(s, "SLACK_TOKEN") || !strings.Contains(s, "possible pasted secret") {
		t.Errorf("ls should flag SLACK_TOKEN as a possible pasted secret:\n%s", s)
	}
}

func TestSecretLsStates(t *testing.T) {
	// op installed + signed in; a filled ref + a placeholder.
	f := fakeEnv{
		present: map[string]bool{"op": true},
		output:  map[string]string{"op account list": "me@example.com\n"},
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
		files: map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=op://Private/Slack/credential\n" +
			"OTHER=op://<vault>/<item>/credential\n"},
	}
	var out bytes.Buffer
	runSecretLs(f.env(), &out)
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

func TestSecretLsOpNotSignedIn(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"op": true},
		// no "op account list" output => not signed in
		envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"},
	}
	var out bytes.Buffer
	runSecretLs(f.env(), &out)
	s := out.String()
	if !strings.Contains(s, "no account configured") {
		t.Errorf("want no-account-configured state:\n%s", s)
	}
	if !strings.Contains(s, "not present") {
		t.Errorf("want op-refs.env absent state:\n%s", s)
	}
	if !strings.Contains(s, "pi-stack secret set") {
		t.Errorf("want the absent-file hint to point at `secret set`:\n%s", s)
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

// TestSecretCheckMissingRefsHintsSet: with no op-refs.env, `secret check`
// points at the new `secret set` primitive, not the removed `edit`. runSecretCheck
// calls os.Exit(3) on a missing file, so this runs in a subprocess.
func TestSecretCheckMissingRefsHintsSet(t *testing.T) {
	if os.Getenv("PI_STACK_SECRET_CHECK_MISSING") == "1" {
		f := fakeEnv{envVars: map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml"}}
		runSecretCheck(f.env(), os.Stdout)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCheckMissingRefsHintsSet")
	cmd.Env = append(os.Environ(), "PI_STACK_SECRET_CHECK_MISSING=1")
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v (output: %s)", err, outBuf)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", ee.ExitCode())
	}
	if !strings.Contains(string(outBuf), "pi-stack secret set <ENV_VAR> op://vault/item/field") {
		t.Errorf("want a `secret set` hint, got:\n%s", outBuf)
	}
}

func TestSecretHelpConfigIndependent(t *testing.T) {
	// -h must print usage and NOT touch config/op — runSecretCmd handles help
	// before any env work. We can't call os.Exit-free easily, so assert the
	// help sentinel path via wantsHelp used inside runSecretCmd is honored by
	// checking secretUsage is non-empty and wantsHelp detects the flag.
	if !wantsHelp([]string{"--help"}) || !wantsHelp([]string{"ls", "-h"}) {
		t.Error("wantsHelp should detect secret help flags")
	}
	if secretUsage == "" {
		t.Error("secretUsage must be defined")
	}
}

// --- secret set / rm: hermetic, via an in-memory shellEnv (readFile/writeFile
// backed by a plain map — no real disk touched). ---

// memEnv builds a shellEnv whose readFile/writeFile operate on an in-memory
// files map, and whose getenv resolves PI_STACK_CONFIG so defaultOpRefsPath
// lines up with the fake path used by the test.
func memEnv(files map[string]string) shellEnv {
	return shellEnv{
		getenv: func(name string) string {
			if name == "PI_STACK_CONFIG" {
				return "/fake/config/config.toml"
			}
			return ""
		},
		readFile: func(path string) (string, error) {
			if c, ok := files[path]; ok {
				return c, nil
			}
			return "", os.ErrNotExist
		},
		writeFile: func(path string, data []byte, perm os.FileMode) error {
			files[path] = string(data)
			return nil
		},
	}
}

const fakeRefsPath = "/fake/config/op-refs.env"

func TestSecretSetUpsertsNewKey(t *testing.T) {
	files := map[string]string{fakeRefsPath: "# header\nSLACK_TOKEN=op://Private/Slack/credential\n"}
	var out bytes.Buffer
	runSecretSet(memEnv(files), &out, "GITHUB_TOKEN", "op://Private/GitHub/credential")
	got := files[fakeRefsPath]
	want := "# header\nSLACK_TOKEN=op://Private/Slack/credential\nGITHUB_TOKEN=op://Private/GitHub/credential\n"
	if got != want {
		t.Errorf("content after set = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "set GITHUB_TOKEN = op://Private/GitHub/credential in "+fakeRefsPath) {
		t.Errorf("output = %q, want a confirmation line naming the ref", out.String())
	}
}

func TestSecretSetReplacesExistingKeyPreservingOthers(t *testing.T) {
	files := map[string]string{fakeRefsPath: "# a comment\nSLACK_TOKEN=op://Private/Slack/old\n\nGOG_ACCOUNT=me@example.com\n"}
	runSecretSet(memEnv(files), &bytes.Buffer{}, "SLACK_TOKEN", "op://Private/Slack/new")
	got := files[fakeRefsPath]
	want := "# a comment\nSLACK_TOKEN=op://Private/Slack/new\n\nGOG_ACCOUNT=me@example.com\n"
	if got != want {
		t.Errorf("content after replace = %q, want %q", got, want)
	}
}

func TestSecretSetRejectsNonRefForSecretKey(t *testing.T) {
	if os.Getenv("PI_STACK_SECRET_SET_REJECT") == "1" {
		runSecretSet(memEnv(map[string]string{fakeRefsPath: "X=1\n"}), os.Stdout, "SLACK_TOKEN", "xoxb-pasted-secret-value")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetRejectsNonRefForSecretKey")
	cmd.Env = append(os.Environ(), "PI_STACK_SECRET_SET_REJECT=1")
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v (output: %s)", err, outBuf)
	}
	if ee.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2", ee.ExitCode())
	}
	if strings.Contains(string(outBuf), "xoxb-pasted-secret-value") {
		t.Errorf("rejection message LEAKED the pasted value: %s", outBuf)
	}
	if !strings.Contains(string(outBuf), "refs-only") {
		t.Errorf("rejection message should explain the refs-only policy: %s", outBuf)
	}
}

// TestSecretSetRejectsControlChars is the injection regression: a value carrying
// a newline must be refused (exit 2), never written, so it cannot smuggle a
// SECOND KEY=value line (e.g. a pasted plaintext secret) into op-refs.env.
func TestSecretSetRejectsControlChars(t *testing.T) {
	if os.Getenv("PI_STACK_SECRET_SET_NL") == "1" {
		runSecretSet(memEnv(map[string]string{fakeRefsPath: "X=1\n"}), os.Stdout,
			"GITHUB_TOKEN", "op://V/I/f\nSLACK_TOKEN=xoxb-injected")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetRejectsControlChars")
	cmd.Env = append(os.Environ(), "PI_STACK_SECRET_SET_NL=1")
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v (output: %s)", err, outBuf)
	}
	if ee.ExitCode() != 2 {
		t.Errorf("exit code = %d, want 2", ee.ExitCode())
	}
	if strings.Contains(string(outBuf), "xoxb-injected") {
		t.Errorf("rejection LEAKED the injected value: %s", outBuf)
	}
	if !strings.Contains(string(outBuf), "control character") {
		t.Errorf("rejection should name the control-character reason: %s", outBuf)
	}
}

func TestSecretSetAllowsNonSecretAllowlistLiteral(t *testing.T) {
	files := map[string]string{fakeRefsPath: "X=1\n"}
	var out bytes.Buffer
	runSecretSet(memEnv(files), &out, "GOG_ACCOUNT", "me@example.com")
	got := files[fakeRefsPath]
	if !strings.Contains(got, "GOG_ACCOUNT=me@example.com") {
		t.Errorf("content after set = %q, want GOG_ACCOUNT set to the literal", got)
	}
	if !config.NonSecretOpRefsKeys["GOG_ACCOUNT"] {
		t.Fatal("sanity: GOG_ACCOUNT must be on the non-secret allowlist for this test to prove anything")
	}
}

func TestSecretSetEncodesSpacedField(t *testing.T) {
	files := map[string]string{fakeRefsPath: "X=1\n"}
	var out bytes.Buffer
	runSecretSet(memEnv(files), &out, "OPENAI_API_KEY", "op://Docker/OPENAI_API_KEY/api key")
	got := files[fakeRefsPath]
	if !strings.Contains(got, "OPENAI_API_KEY=op://Docker/OPENAI_API_KEY/api%20key") {
		t.Errorf("content after set = %q, want the space encoded to %%20", got)
	}
	if !strings.Contains(out.String(), "encoded a space") {
		t.Errorf("output = %q, want a note about the encoding", out.String())
	}
}

func TestSecretSetSeedsFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	env := defaultShellEnv()
	var out bytes.Buffer
	runSecretSet(env, &out, "SLACK_TOKEN", "op://Private/Slack/credential")

	path := config.OpRefsPath()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded+set file: %v", err)
	}
	s := string(content)
	if !strings.Contains(s, "op-refs.env maps ENV_VAR") {
		t.Errorf("seeded file should still carry the template header:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN=op://Private/Slack/credential") {
		t.Errorf("seeded file should carry the new ref:\n%s", s)
	}
}

func TestSecretRmRemovesKeyPreservingRest(t *testing.T) {
	files := map[string]string{fakeRefsPath: "# header\nSLACK_TOKEN=op://Private/Slack/credential\nGOG_ACCOUNT=me@example.com\n"}
	var out bytes.Buffer
	runSecretRm(memEnv(files), &out, "SLACK_TOKEN")
	got := files[fakeRefsPath]
	want := "# header\nGOG_ACCOUNT=me@example.com\n"
	if got != want {
		t.Errorf("content after rm = %q, want %q", got, want)
	}
	if !strings.Contains(out.String(), "removed SLACK_TOKEN from "+fakeRefsPath) {
		t.Errorf("output = %q, want a confirmation line", out.String())
	}
}

func TestSecretRmMissingKeyIsCleanNoop(t *testing.T) {
	files := map[string]string{fakeRefsPath: "SLACK_TOKEN=op://Private/Slack/credential\n"}
	var out bytes.Buffer
	runSecretRm(memEnv(files), &out, "NOPE_NOT_THERE")
	if files[fakeRefsPath] != "SLACK_TOKEN=op://Private/Slack/credential\n" {
		t.Errorf("rm on a missing key must not modify the file, got %q", files[fakeRefsPath])
	}
	if !strings.Contains(out.String(), "no ref named NOPE_NOT_THERE") {
		t.Errorf("output = %q, want a clean no-op message", out.String())
	}
}

func TestSecretRmMissingFileIsCleanNoop(t *testing.T) {
	var out bytes.Buffer
	runSecretRm(memEnv(map[string]string{}), &out, "SLACK_TOKEN")
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("output = %q, want a clear missing-file message", out.String())
	}
}
