package secret

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/hostenv/hostenvtest"
	"pix/host/sys"
	"pix/host/sys/systest"
)

func TestParseOpRefsClassification(t *testing.T) {
	content := `# a comment
SLACK_TOKEN=op://Private/Slack/credential
UNFILLED=op://<vault>/<item>/credential
GOG_ACCOUNT=me@example.com
PASTED=xoxb-123-secret
`
	refs := ParseOpRefs(content)
	byKey := map[string]OpRef{}
	for _, r := range refs {
		byKey[r.Key] = r
	}
	if r := byKey["SLACK_TOKEN"]; !r.IsRef || r.Placeholder {
		t.Errorf("SLACK_TOKEN: isRef=%v placeholder=%v, want filled ref", r.IsRef, r.Placeholder)
	}
	if r := byKey["UNFILLED"]; !r.IsRef || !r.Placeholder {
		t.Errorf("UNFILLED: isRef=%v placeholder=%v, want unfilled placeholder", r.IsRef, r.Placeholder)
	}
	if r := byKey["GOG_ACCOUNT"]; !r.NonSecret {
		t.Errorf("GOG_ACCOUNT should be on the non-secret allowlist")
	}
	if r := byKey["PASTED"]; r.IsRef || r.NonSecret {
		t.Errorf("PASTED literal: isRef=%v nonSecret=%v, want neither", r.IsRef, r.NonSecret)
	}
}

// TestSeededOpRefsHasNoActiveEntries covers F1: a freshly seeded op-refs.env has
// ZERO active (uncommented) ref lines — ParseOpRefs finds no entries.
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
	if refs := ParseOpRefs(string(content)); len(refs) != 0 {
		t.Errorf("freshly seeded op-refs.env has %d active entries, want 0: %+v", len(refs), refs)
	}
	for i, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") && opRefLineKey(line) == "" {
			t.Errorf("template line %d is invalid dotenv prose: %q", i+1, line)
		}
	}
}

func TestRepairLegacyOpRefsTemplate(t *testing.T) {
	in := "# header\nhost MCP server it resolves those refs from 1Password and injects them as env\n" +
		"vars — the secret never touches disk or the sandbox. A server with no creds\n" +
		"(pio) needs no entry.\nEXAMPLE_KEY=op://Vault/Item/field\n"
	got, changed := repairLegacyOpRefsTemplate(in)
	if !changed {
		t.Fatal("expected legacy template repair")
	}
	if !strings.Contains(got, "# host MCP server") || !strings.Contains(got, "EXAMPLE_KEY=op://Vault/Item/field") {
		t.Fatalf("repair did not preserve/refactor expected lines:\n%s", got)
	}
	if again, changed := repairLegacyOpRefsTemplate(got); changed || again != got {
		t.Fatal("repair must be idempotent")
	}
}

// TestSecretLsShortLiteralFlagged covers F4 parity in `secret ls`: a short,
// NOT-secret-shaped literal is still flagged (refs-only) and its value is never
// printed.
func TestSecretLsShortLiteralFlagged(t *testing.T) {
	const val = "correcthorsebattery"
	f := hostenvtest.Env{
		Present: map[string]bool{},
		EnvVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		Files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=" + val + "\n"},
	}
	var out bytes.Buffer
	RunSecretLs(f.Build(), &out)
	s := out.String()
	if strings.Contains(s, val) {
		t.Errorf("secret ls LEAKED the literal value:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN") || !strings.Contains(s, "not an op:// ref") {
		t.Errorf("ls should flag the short literal as not-a-ref:\n%s", s)
	}
}

func TestHasPlaceholder(t *testing.T) {
	if !HasPlaceholder("op://<vault>/x/y") {
		t.Error("angle-bracket placeholder not detected")
	}
	if HasPlaceholder("op://Private/Slack/credential") {
		t.Error("a filled ref wrongly flagged as placeholder")
	}
}

// TestSecretLsNeverLeaksValue is the security gate: a pasted secret value must
// NEVER appear in `secret ls` output.
func TestSecretLsNeverLeaksValue(t *testing.T) {
	const pasted = "xoxb-THIS-MUST-NOT-BE-PRINTED"
	f := hostenvtest.Env{
		Present: map[string]bool{}, // op not installed
		EnvVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		Files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=" + pasted + "\n"},
	}
	var out bytes.Buffer
	RunSecretLs(f.Build(), &out)
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
	f := hostenvtest.Env{
		Present: map[string]bool{"op": true},
		Output:  map[string]string{"op account list": "me@example.com\n"},
		EnvVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		Files: map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=op://Private/Slack/credential\n" +
			"OTHER=op://<vault>/<item>/credential\n"},
	}
	var out bytes.Buffer
	RunSecretLs(f.Build(), &out)
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
	f := hostenvtest.Env{
		Present: map[string]bool{"op": true},
		// no "op account list" output => not signed in
		EnvVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
	}
	var out bytes.Buffer
	RunSecretLs(f.Build(), &out)
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
	f := hostenvtest.Env{
		Present: map[string]bool{"op": true},
		Output: map[string]string{
			"op account list":                       "me@example.com\n",
			"op read op://Private/Slack/credential": resolved,
		},
		EnvVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		Files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=op://Private/Slack/credential\n"},
	}
	var out bytes.Buffer
	RunSecretCheck(f.Build(), &out)
	s := out.String()
	if strings.Contains(s, resolved) {
		t.Errorf("secret check LEAKED the resolved value:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN: OK") {
		t.Errorf("want SLACK_TOKEN OK:\n%s", s)
	}
}

// TestSecretCheckMissingRefsHintsSet: with no op-refs.env, `secret check`
// points at the new `secret set` primitive, not the removed `edit`. RunSecretCheck
// calls os.Exit(3) on a missing file, so this runs in a subprocess.
func TestSecretCheckMissingRefsHintsSet(t *testing.T) {
	if os.Getenv("PIX_SECRET_CHECK_MISSING") == "1" {
		f := hostenvtest.Env{EnvVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"}}
		RunSecretCheck(f.Build(), os.Stdout)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretCheckMissingRefsHintsSet")
	cmd.Env = append(os.Environ(), "PIX_SECRET_CHECK_MISSING=1")
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", err, outBuf)
	}
	if ee.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", ee.ExitCode())
	}
	if !strings.Contains(string(outBuf), "pix secret set <ENV_VAR> op://vault/item/field") {
		t.Errorf("want a `secret set` hint, got:\n%s", outBuf)
	}
}

// --- secret set / rm: hermetic, via an in-memory hostenv.Env (readFile/writeFile
// backed by a plain map — no real disk touched). ---

// memEnv builds a hostenv.Env whose readFile/writeFile operate on an in-memory
// files map, and whose getenv resolves PIX_CONFIG so DefaultOpRefsPath
// lines up with the fake path used by the test.
func memEnv(files map[string]string) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{GetenvFn: func(name string) string {
		if name == "PIX_CONFIG" {
			return "/fake/config/config.toml"
		}
		return ""
	}, ReadFileFn: func(path string) (string, error) {
		if c, ok := files[path]; ok {
			return c, nil
		}
		return "", os.ErrNotExist
	}, WriteFileFn: func(path string, data []byte, perm os.FileMode) error {
		files[path] = string(data)
		return nil
	}}}
}

const fakeRefsPath = "/fake/config/op-refs.env"

func TestSecretSetUpsertsNewKey(t *testing.T) {
	files := map[string]string{fakeRefsPath: "# header\nSLACK_TOKEN=op://Private/Slack/credential\n"}
	var out bytes.Buffer
	RunSecretSet(memEnv(files), &out, "GITHUB_TOKEN", "op://Private/GitHub/credential")
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
	RunSecretSet(memEnv(files), &bytes.Buffer{}, "SLACK_TOKEN", "op://Private/Slack/new")
	got := files[fakeRefsPath]
	want := "# a comment\nSLACK_TOKEN=op://Private/Slack/new\n\nGOG_ACCOUNT=me@example.com\n"
	if got != want {
		t.Errorf("content after replace = %q, want %q", got, want)
	}
}

func TestSecretSetRejectsNonRefForSecretKey(t *testing.T) {
	if os.Getenv("PIX_SECRET_SET_REJECT") == "1" {
		RunSecretSet(memEnv(map[string]string{fakeRefsPath: "X=1\n"}), os.Stdout, "SLACK_TOKEN", "xoxb-pasted-secret-value")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetRejectsNonRefForSecretKey")
	cmd.Env = append(os.Environ(), "PIX_SECRET_SET_REJECT=1")
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", err, outBuf)
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
		RunSecretSet(memEnv(map[string]string{fakeRefsPath: "X=1\n"}), os.Stdout,
			"GITHUB_TOKEN", "op://V/I/f\nSLACK_TOKEN=xoxb-injected")
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run", "TestSecretSetRejectsControlChars")
	cmd.Env = append(os.Environ(), "PIX_SECRET_SET_NL=1")
	outBuf, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected an ExitError, got %v (Output: %s)", err, outBuf)
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
	RunSecretSet(memEnv(files), &out, "GOG_ACCOUNT", "me@example.com")
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
	RunSecretSet(memEnv(files), &out, "OPENAI_API_KEY", "op://Docker/OPENAI_API_KEY/api key")
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

// Regression: `pix secret set` given a %20-encoded ref (the value the OLD
// help/man guidance told users to type) must normalize it to a literal space
// in op-refs.env — op 2.35.0 rejects a percent-encoded ref outright, so the
// stored ref would never resolve.
func TestSecretSetNormalizesPercentEncodedSpaceToLiteral(t *testing.T) {
	files := map[string]string{fakeRefsPath: ""}
	env := memEnv(files)
	var out bytes.Buffer
	RunSecretSet(env, &out, "ANTHROPIC_API_KEY", "op://Vault/Item/api%20key")

	opRefs := files[fakeRefsPath]
	if !strings.Contains(opRefs, "ANTHROPIC_API_KEY=op://Vault/Item/api key") {
		t.Errorf("op-refs.env = %q, want the %%20 normalized to a literal space", opRefs)
	}
	if strings.Contains(opRefs, "api%20key") {
		t.Errorf("op-refs.env = %q, must NOT keep the percent-encoded space", opRefs)
	}
}

func TestSecretSetSeedsFileWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	env := hostenv.Env{System: sys.Real{}}
	var out bytes.Buffer
	RunSecretSet(env, &out, "SLACK_TOKEN", "op://Private/Slack/credential")

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
	RunSecretRm(memEnv(files), &out, "SLACK_TOKEN")
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
	RunSecretRm(memEnv(files), &out, "NOPE_NOT_THERE")
	if files[fakeRefsPath] != "SLACK_TOKEN=op://Private/Slack/credential\n" {
		t.Errorf("rm on a missing key must not modify the file, got %q", files[fakeRefsPath])
	}
	if !strings.Contains(out.String(), "no ref named NOPE_NOT_THERE") {
		t.Errorf("output = %q, want a clean no-op message", out.String())
	}
}

func TestSecretRmMissingFileIsCleanNoop(t *testing.T) {
	var out bytes.Buffer
	RunSecretRm(memEnv(map[string]string{}), &out, "SLACK_TOKEN")
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("output = %q, want a clear missing-file message", out.String())
	}
}

func TestSecretSetAndRmFailOnUnreadableOpRefs(t *testing.T) {
	env := memEnv(map[string]string{})
	systest.Of(env.System).ReadFileFn = func(string) (string, error) { return "", os.ErrPermission }

	var setOut bytes.Buffer
	if err := RunSecretSet(env, &setOut, "SLACK_TOKEN", "op://v/slack/token"); err == nil {
		t.Fatal("secret set must fail when op-refs.env cannot be read")
	}
	if !strings.Contains(setOut.String(), "could not read") {
		t.Errorf("set output = %q, want an explicit read failure", setOut.String())
	}

	var rmOut bytes.Buffer
	if err := RunSecretRm(env, &rmOut, "SLACK_TOKEN"); err == nil {
		t.Fatal("secret rm must fail when op-refs.env cannot be read")
	}
	if !strings.Contains(rmOut.String(), "could not read") {
		t.Errorf("rm output = %q, want an explicit read failure", rmOut.String())
	}
}

// TestSecretSetThenRm_FullLifecycle: `secret set` lands a provider key in
// op-refs.env and a subsequent `secret rm` fully undoes it — the full round
// trip, not just each half in isolation.
func TestSecretSetThenRm_FullLifecycle(t *testing.T) {
	files := map[string]string{fakeRefsPath: ""}
	env := memEnv(files)

	var setOut bytes.Buffer
	if err := RunSecretSet(env, &setOut, "OPENAI_API_KEY", "op://v/openai/key"); err != nil {
		t.Fatalf("set: unexpected error: %v", err)
	}
	if !strings.Contains(files[fakeRefsPath], "OPENAI_API_KEY") {
		t.Fatalf("set must land the ref in op-refs.env: %q", files[fakeRefsPath])
	}

	var rmOut bytes.Buffer
	if err := RunSecretRm(env, &rmOut, "OPENAI_API_KEY"); err != nil {
		t.Fatalf("rm: unexpected error: %v", err)
	}
	if strings.Contains(files[fakeRefsPath], "OPENAI_API_KEY") {
		t.Errorf("rm must remove the ref from op-refs.env, got %q", files[fakeRefsPath])
	}
	if !strings.Contains(setOut.String(), "op://v/openai/key") {
		t.Error("the set confirmation should echo the ref (refs are safe to print)")
	}
}
