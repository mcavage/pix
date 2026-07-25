package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
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
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
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
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
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
	if os.Getenv("PIX_SECRET_BOGUS") == "1" {
		runSecretCmd([]string{"check", "--bogus"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCheckRejectsTrailingArg")
	cmd.Env = append(os.Environ(), "PIX_SECRET_BOGUS=1")
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
			if os.Getenv("PIX_SECRET_ARGCOUNT") == tc.name {
				runSecretCmd(tc.argv)
				return
			}
			cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCmdArgCounts/"+strings.ReplaceAll(tc.name, " ", "_"))
			cmd.Env = append(os.Environ(), "PIX_SECRET_ARGCOUNT="+tc.name)
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
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
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
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
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
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
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
	if !strings.Contains(s, "pix secret set") {
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
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
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
	if os.Getenv("PIX_SECRET_CHECK_MISSING") == "1" {
		f := fakeEnv{envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"}}
		runSecretCheck(f.env(), os.Stdout)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCheckMissingRefsHintsSet")
	cmd.Env = append(os.Environ(), "PIX_SECRET_CHECK_MISSING=1")
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v (output: %s)", err, outBuf)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", ee.ExitCode())
	}
	if !strings.Contains(string(outBuf), "pix secret set <ENV_VAR> op://vault/item/field") {
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
// files map, and whose getenv resolves PIX_CONFIG so defaultOpRefsPath
// lines up with the fake path used by the test.
func memEnv(files map[string]string) shellEnv {
	return shellEnv{
		getenv: func(name string) string {
			if name == "PIX_CONFIG" {
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
	if os.Getenv("PIX_SECRET_SET_REJECT") == "1" {
		runSecretSet(memEnv(map[string]string{fakeRefsPath: "X=1\n"}), os.Stdout, "SLACK_TOKEN", "xoxb-pasted-secret-value")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetRejectsNonRefForSecretKey")
	cmd.Env = append(os.Environ(), "PIX_SECRET_SET_REJECT=1")
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
	if os.Getenv("PIX_SECRET_SET_NL") == "1" {
		runSecretSet(memEnv(map[string]string{fakeRefsPath: "X=1\n"}), os.Stdout,
			"GITHUB_TOKEN", "op://V/I/f\nSLACK_TOKEN=xoxb-injected")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetRejectsControlChars")
	cmd.Env = append(os.Environ(), "PIX_SECRET_SET_NL=1")
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

func TestUpsertOpRefCanonicalizesConflictingDuplicates(t *testing.T) {
	got := upsertOpRef(
		"ANTHROPIC_API_KEY=op://vault/old/key\nX=1\nANTHROPIC_API_KEY=op://vault/stale/key\n",
		"ANTHROPIC_API_KEY",
		"op://vault/new/key",
	)
	want := "ANTHROPIC_API_KEY=op://vault/new/key\nX=1\n"
	if got != want {
		t.Fatalf("upsert with duplicates = %q, want %q", got, want)
	}
}

func TestSecretSetKeepsLiteralSpacedField(t *testing.T) {
	files := map[string]string{fakeRefsPath: "X=1\n"}
	var out bytes.Buffer
	runSecretSet(memEnv(files), &out, "OPENAI_API_KEY", "op://Docker/OPENAI_API_KEY/api key")
	got := files[fakeRefsPath]
	if !strings.Contains(got, "OPENAI_API_KEY=op://Docker/OPENAI_API_KEY/api key") {
		t.Errorf("content after set = %q, want the space kept literal", got)
	}
	if strings.Contains(got, "api%20key") {
		t.Errorf("content after set = %q, must NOT percent-encode the space (op read/op run reject %%20)", got)
	}
	if strings.Contains(out.String(), "encoded a space") {
		t.Errorf("output = %q, must not claim it encoded a space", out.String())
	}
}

// item 7: `pix secret set` for a provider key mirrors it into
// hostmode.env too, not just op-refs.env, so a single `secret set` per
// provider is really enough to wire BOTH the sandbox and host mode.
func TestSecretSetMirrorsProviderKeyToHostMode(t *testing.T) {
	files := map[string]string{fakeRefsPath: ""}
	env := memEnv(files)
	var out bytes.Buffer
	runSecretSet(env, &out, "ANTHROPIC_API_KEY", "op://v/anthropic/key")
	hostMode, err := env.readFile(hostModeRefsPath(env))
	if err != nil {
		t.Fatalf("read hostmode.env: %v", err)
	}
	if !strings.Contains(hostMode, "ANTHROPIC_API_KEY=op://v/anthropic/key") {
		t.Errorf("hostmode.env = %q, want the mirrored ref", hostMode)
	}
	if !strings.Contains(files[fakeRefsPath], "ANTHROPIC_API_KEY=op://v/anthropic/key") {
		t.Errorf("op-refs.env = %q, want the ref too", files[fakeRefsPath])
	}
}

// A non-provider key (e.g. SLACK_TOKEN) is NOT mirrored to hostmode.env —
// only the three model-provider keys are.
func TestSecretSetDoesNotMirrorNonProviderKeys(t *testing.T) {
	files := map[string]string{fakeRefsPath: ""}
	env := memEnv(files)
	var out bytes.Buffer
	runSecretSet(env, &out, "SLACK_TOKEN", "op://v/slack/token")
	if _, err := env.readFile(hostModeRefsPath(env)); err == nil {
		t.Error("a non-provider key must not create/mirror into hostmode.env")
	}
}

func TestSecretSetSeedsFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
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

func TestSecretSetAndRmFailOnUnreadableOpRefs(t *testing.T) {
	env := memEnv(map[string]string{})
	env.readFile = func(string) (string, error) { return "", os.ErrPermission }

	var setOut bytes.Buffer
	if err := runSecretSet(env, &setOut, "SLACK_TOKEN", "op://v/slack/token"); err == nil {
		t.Fatal("secret set must fail when op-refs.env cannot be read")
	}
	if !strings.Contains(setOut.String(), "could not read") {
		t.Errorf("set output = %q, want an explicit read failure", setOut.String())
	}

	var rmOut bytes.Buffer
	if err := runSecretRm(env, &rmOut, "SLACK_TOKEN"); err == nil {
		t.Fatal("secret rm must fail when op-refs.env cannot be read")
	}
	if !strings.Contains(rmOut.String(), "could not read") {
		t.Errorf("rm output = %q, want an explicit read failure", rmOut.String())
	}
}

// --- item 1: `secret set`/`secret rm` exit codes + dual-file provider-key
// lifecycle -------------------------------------------------------------

// TestSecretSetMirrorFailure_DispatcherExitsNonzero: a hostmode.env mirror
// failure for a provider key must make the CLI exit nonzero, never quietly
// succeed. Runs through the REAL dispatcher (runSecretCmd) against the real
// filesystem in a subprocess, so the exit code is genuinely observed rather
// than merely a returned error value nobody acted on.
func TestSecretSetMirrorFailure_DispatcherExitsNonzero(t *testing.T) {
	if cfgDir := os.Getenv("PIX_SECRET_SET_MIRROR_FAIL_CFGDIR"); cfgDir != "" {
		runSecretCmd([]string{"set", "ANTHROPIC_API_KEY", "op://v/anthropic/key"})
		return
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	// Sabotage hostmode.env: pre-create it as a DIRECTORY, so the atomic
	// rename-into-place mirror write fails (EISDIR), while op-refs.env (which
	// doesn't exist yet, and gets freshly seeded) stays a normal writable file.
	if err := os.MkdirAll(filepath.Join(cfgDir, "hostmode.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetMirrorFailure_DispatcherExitsNonzero")
	cmd.Env = append(os.Environ(),
		"PIX_SECRET_SET_MIRROR_FAIL_CFGDIR="+cfgDir,
		"PIX_CONFIG="+filepath.Join(cfgDir, "config.toml"),
	)
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError (a mirror failure must exit nonzero), got %v (output: %s)", err, outBuf)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("exit code = 0, want nonzero, output: %s", outBuf)
	}
	if !strings.Contains(string(outBuf), "could not mirror") {
		t.Errorf("output should explain the mirror failure, got:\n%s", outBuf)
	}
}

// A provider key present in BOTH op-refs.env and hostmode.env is removed from
// BOTH by a single `secret rm`.
func TestSecretRm_ProviderKey_RemovesFromBothFiles(t *testing.T) {
	files := map[string]string{
		fakeRefsPath: "ANTHROPIC_API_KEY=op://v/anthropic/key\nSLACK_TOKEN=op://v/slack/token\n",
	}
	env := memEnv(files)
	hmPath := hostModeRefsPath(env)
	if err := env.writeFile(hmPath, []byte("ANTHROPIC_API_KEY=op://v/anthropic/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runSecretRm(env, &out, "ANTHROPIC_API_KEY"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(files[fakeRefsPath], "ANTHROPIC_API_KEY") {
		t.Errorf("op-refs.env still has the key: %q", files[fakeRefsPath])
	}
	if !strings.Contains(files[fakeRefsPath], "SLACK_TOKEN") {
		t.Errorf("op-refs.env must preserve the untouched entry: %q", files[fakeRefsPath])
	}
	if strings.Contains(files[hmPath], "ANTHROPIC_API_KEY") {
		t.Errorf("hostmode.env still has the key: %q", files[hmPath])
	}
	if !strings.Contains(out.String(), "removed ANTHROPIC_API_KEY from "+fakeRefsPath+" and "+hmPath) {
		t.Errorf("output = %q, want a confirmation naming both files", out.String())
	}
}

// A non-provider key (e.g. SLACK_TOKEN) is removed ONLY from op-refs.env,
// exactly as before this feature — hostmode.env is never even consulted.
func TestSecretRm_NonProviderKey_OnlyTouchesOpRefs(t *testing.T) {
	files := map[string]string{fakeRefsPath: "SLACK_TOKEN=op://v/slack/token\n"}
	env := memEnv(files)
	hmPath := hostModeRefsPath(env)
	if err := env.writeFile(hmPath, []byte("SLACK_TOKEN=op://v/slack/token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runSecretRm(env, &out, "SLACK_TOKEN"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(files[fakeRefsPath], "SLACK_TOKEN") {
		t.Error("op-refs.env should no longer have SLACK_TOKEN")
	}
	// hostmode.env is untouched: a non-provider key was never mirrored there in
	// the first place, so rm must not touch it either.
	if !strings.Contains(files[hmPath], "SLACK_TOKEN") {
		t.Error("hostmode.env must be left untouched for a non-provider key")
	}
}

// A partial failure — op-refs.env removal succeeds, but the hostmode.env
// write fails — is reported HONESTLY (names both what succeeded and what
// didn't) and returns a non-nil error, never a silent success.
func TestSecretRm_ProviderKey_PartialFailure_HostModeWriteFails(t *testing.T) {
	files := map[string]string{
		fakeRefsPath: "ANTHROPIC_API_KEY=op://v/anthropic/key\n",
	}
	env := memEnv(files)
	hmPath := hostModeRefsPath(env)
	if err := env.writeFile(hmPath, []byte("ANTHROPIC_API_KEY=op://v/anthropic/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	realWrite := env.writeFile
	env.writeFile = func(p string, d []byte, m os.FileMode) error {
		if p == hmPath {
			return os.ErrPermission
		}
		return realWrite(p, d, m)
	}
	var out bytes.Buffer
	err := runSecretRm(env, &out, "ANTHROPIC_API_KEY")
	if err == nil {
		t.Fatal("a hostmode.env write failure must return a non-nil error")
	}
	if strings.Contains(files[fakeRefsPath], "ANTHROPIC_API_KEY") {
		t.Errorf("op-refs.env removal (which succeeded) must still take effect: %q", files[fakeRefsPath])
	}
	if !strings.Contains(out.String(), "removed ANTHROPIC_API_KEY from "+fakeRefsPath) || !strings.Contains(out.String(), "could not remove it from") {
		t.Errorf("output must honestly report the partial failure, got:\n%s", out.String())
	}
}

// TestSecretRm_DispatcherExitsNonzeroOnPartialFailure exercises the same
// partial failure through the real dispatcher, proving the CLI itself exits
// nonzero (not merely that runSecretRm returns an error nobody consumed).
func TestSecretRm_DispatcherExitsNonzeroOnPartialFailure(t *testing.T) {
	if cfgDir := os.Getenv("PIX_SECRET_RM_PARTIAL_FAIL_CFGDIR"); cfgDir != "" {
		runSecretCmd([]string{"rm", "ANTHROPIC_API_KEY"})
		return
	}
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "op-refs.env"), []byte("ANTHROPIC_API_KEY=op://v/anthropic/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// hostmode.env exists with the same key, but AS A DIRECTORY so the rename-in
	// removal write fails (EISDIR).
	if err := os.MkdirAll(filepath.Join(cfgDir, "hostmode.env"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretRm_DispatcherExitsNonzeroOnPartialFailure")
	cmd.Env = append(os.Environ(),
		"PIX_SECRET_RM_PARTIAL_FAIL_CFGDIR="+cfgDir,
		"PIX_CONFIG="+filepath.Join(cfgDir, "config.toml"),
	)
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError (a partial rm failure must exit nonzero), got %v (output: %s)", err, outBuf)
	}
	if ee.ExitCode() == 0 {
		t.Errorf("exit code = 0, want nonzero, output: %s", outBuf)
	}
}

// TestSecretSetThenRm_FullLifecycle: `secret set` mirrors a provider key into
// both files, and a subsequent `secret rm` fully undoes it in both — the
// full round trip, not just each half in isolation.
func TestSecretSetThenRm_FullLifecycle(t *testing.T) {
	files := map[string]string{fakeRefsPath: ""}
	env := memEnv(files)
	hmPath := hostModeRefsPath(env)

	var setOut bytes.Buffer
	if err := runSecretSet(env, &setOut, "OPENAI_API_KEY", "op://v/openai/key"); err != nil {
		t.Fatalf("set: unexpected error: %v", err)
	}
	if !strings.Contains(files[fakeRefsPath], "OPENAI_API_KEY") || !strings.Contains(files[hmPath], "OPENAI_API_KEY") {
		t.Fatalf("set must land the ref in both files: op-refs=%q hostmode=%q", files[fakeRefsPath], files[hmPath])
	}

	var rmOut bytes.Buffer
	if err := runSecretRm(env, &rmOut, "OPENAI_API_KEY"); err != nil {
		t.Fatalf("rm: unexpected error: %v", err)
	}
	if strings.Contains(files[fakeRefsPath], "OPENAI_API_KEY") {
		t.Errorf("rm must remove the ref from op-refs.env, got %q", files[fakeRefsPath])
	}
	if strings.Contains(files[hmPath], "OPENAI_API_KEY") {
		t.Errorf("rm must remove the ref from hostmode.env, got %q", files[hmPath])
	}
	if strings.Contains(setOut.String(), "op://v/openai/key") == false {
		t.Error("the set confirmation should echo the ref (refs are safe to print)")
	}
}
