package secret

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/sys/systest"
)

// --- real fixtures, replacing the retired hostenv/hostenvtest package ------
//
// Every op-facing probe secret.go uses is env.LookPath("op") + env.Run("op",
// ...) (OpInstalled/OpSignedIn), plus env.Getenv/IsFile/ReadFile/HomeDir for
// locating op-refs.env — so a fixture needs only a PATH-isolated bin dir for a
// real "op" executable, plus a real PIX_CONFIG-pointed tempdir, never an
// in-memory call-keyed double.

// realFixture points PIX_CONFIG at a fresh tempdir (so DefaultOpRefsPath
// resolves there exactly like production) and, when opRefs is non-empty,
// writes a REAL op-refs.env with that content. It returns the real env plus
// the real path, so a test can name the file precisely. PATH is isolated to
// an empty dir, so op is absent unless installOp below adds it.
func realFixture(t *testing.T, opRefs string) (hostenv.Env, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("PIX_HOME", dir) // op-refs.env resolves under PIX_HOME alone (QA F5)
	t.Setenv("PATH", t.TempDir())
	path := filepath.Join(dir, "op-refs.env")
	if opRefs != "" {
		if err := os.WriteFile(path, []byte(opRefs), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return hostenv.Env{System: sys.Real{}}, path
}

// installOp writes a REAL "op" executable on PATH. Invoked with argv, it
// answers with the entry in output keyed by the space-joined argv and exits
// 0; any other invocation exits 1 — an undeclared command fails the test
// loudly, the same contract the retired shared fixture documented.
func installOp(t *testing.T, output map[string]string) {
	t.Helper()
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncase \"$*\" in\n")
	for args, out := range output {
		fmt.Fprintf(&b, "%s)\nprintf %%s %s\nexit 0\n;;\n", shQuote(args), shQuote(out))
	}
	b.WriteString("*) exit 1 ;;\nesac\n")
	if err := os.WriteFile(filepath.Join(dir, "op"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// shQuote single-quotes s for embedding in a POSIX shell script, so neither
// glob metacharacters in a case pattern nor shell syntax in canned output can
// leak out of the literal string a test declared.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// parseOpRefsFixture is the classification corpus: a filled ref, an unfilled
// placeholder, a literal a pack may authorize, and a pasted secret.
const parseOpRefsFixture = `# a comment
SLACK_TOKEN=op://Private/Slack/credential
UNFILLED=op://<vault>/<item>/credential
GOG_ACCOUNT=me@example.com
PASTED=xoxb-123-secret
`

func byKey(refs []OpRef) map[string]OpRef {
	m := map[string]OpRef{}
	for _, r := range refs {
		m[r.Key] = r
	}
	return m
}

func TestParseOpRefsClassification(t *testing.T) {
	// The allowlist is the CALLER's (the active pack's `env_keys`), not a global
	// pix map: GOG_ACCOUNT is a non-secret literal here only because this caller
	// declared it one.
	refs := byKey(ParseOpRefs(parseOpRefsFixture, NonSecret{"GOG_ACCOUNT": true}))
	if r := refs["SLACK_TOKEN"]; !r.IsRef || r.Placeholder {
		t.Errorf("SLACK_TOKEN: isRef=%v placeholder=%v, want filled ref", r.IsRef, r.Placeholder)
	}
	if r := refs["UNFILLED"]; !r.IsRef || !r.Placeholder {
		t.Errorf("UNFILLED: isRef=%v placeholder=%v, want unfilled placeholder", r.IsRef, r.Placeholder)
	}
	if r := refs["GOG_ACCOUNT"]; !r.NonSecret {
		t.Errorf("GOG_ACCOUNT should be non-secret when the caller's allowlist says so")
	}
	if r := refs["PASTED"]; r.IsRef || r.NonSecret {
		t.Errorf("PASTED literal: isRef=%v nonSecret=%v, want neither", r.IsRef, r.NonSecret)
	}
}

// TestParseOpRefsNilAllowlistAllowsNothing is the other half of the new
// mechanism: with a nil allowlist (no active pack, or a caller that only asks
// "is this a ref"), the SAME literal that a pack could authorize is classified
// as not-non-secret. Pix allowlists nothing of its own, so a key is only ever
// a permitted literal because a caller passed it in.
func TestParseOpRefsNilAllowlistAllowsNothing(t *testing.T) {
	refs := byKey(ParseOpRefs(parseOpRefsFixture, nil))
	for _, key := range []string{"GOG_ACCOUNT", "PASTED"} {
		if r := refs[key]; r.NonSecret {
			t.Errorf("%s: NonSecret = true under a nil allowlist; pix allowlists nothing of its own", key)
		}
	}
	// The ref classification is unaffected by the allowlist.
	if r := refs["SLACK_TOKEN"]; !r.IsRef {
		t.Errorf("SLACK_TOKEN: isRef=%v, want a ref regardless of the allowlist", r.IsRef)
	}
}

// TestSeededOpRefsHasNoActiveEntries covers F1: a freshly seeded op-refs.env has
// ZERO active (uncommented) ref lines — ParseOpRefs finds no entries.
func TestSeededOpRefsHasNoActiveEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("PIX_HOME", dir) // op-refs.env resolves under PIX_HOME alone (QA F5)
	// config.SeedOpRefs resolves through config.OpRefsPath (PIX_HOME only, QA
	// F5); PIX_CONFIG above still isolates secret.DefaultOpRefsPath's OWN
	// resolution for the other callers in this file, but this one needs
	// PIX_HOME too, pointed at the SAME dir so both resolve identically.
	t.Setenv("PIX_HOME", dir)
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
	if refs := ParseOpRefs(string(content), nil); len(refs) != 0 {
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
	env, _ := realFixture(t, "SLACK_TOKEN="+val+"\n")
	var out bytes.Buffer
	RunSecretLs(env, &out, nil)
	s := out.String()
	if strings.Contains(s, val) {
		t.Errorf("secret ls LEAKED the literal value:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN") || !strings.Contains(s, "not an op:// ref") {
		t.Errorf("ls should flag the short literal as not-a-ref:\n%s", s)
	}
}

// TestSecretLsAllowlistIsTheCallers: `secret ls` classifies a literal against
// the allowlist it was HANDED, not a global map. The same file reads as a
// pack-authorized non-secret with an allowlist and as a refs-only violation
// without one — so deactivating the pack that authorized a variable takes the
// allowance with it, visibly, in the listing.
func TestSecretLsAllowlistIsTheCallers(t *testing.T) {
	env, _ := realFixture(t, "GOG_ACCOUNT=me@example.com\n")

	var allowed bytes.Buffer
	RunSecretLs(env, &allowed, NonSecret{"GOG_ACCOUNT": true})
	if !strings.Contains(allowed.String(), "GOG_ACCOUNT (non-secret env)") {
		t.Errorf("an allowlisted key should be reported as a non-secret env:\n%s", allowed.String())
	}
	if strings.Contains(allowed.String(), "not an op:// ref") {
		t.Errorf("an allowlisted key must not be flagged refs-only:\n%s", allowed.String())
	}

	var bare bytes.Buffer
	RunSecretLs(env, &bare, nil)
	if !strings.Contains(bare.String(), "not an op:// ref") {
		t.Errorf("without an allowlist the SAME literal must be flagged refs-only:\n%s", bare.String())
	}
	if strings.Contains(bare.String(), "non-secret env") {
		t.Errorf("nothing is non-secret without a caller saying so:\n%s", bare.String())
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
	env, _ := realFixture(t, "SLACK_TOKEN="+pasted+"\n") // op not installed
	var out bytes.Buffer
	RunSecretLs(env, &out, nil)
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
	env, _ := realFixture(t, "SLACK_TOKEN=op://Private/Slack/credential\n"+
		"OTHER=op://<vault>/<item>/credential\n")
	installOp(t, map[string]string{"account list": "me@example.com\n"})
	var out bytes.Buffer
	RunSecretLs(env, &out, nil)
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
	env, _ := realFixture(t, "")
	installOp(t, nil) // present, but no "account list" answer => not signed in
	var out bytes.Buffer
	RunSecretLs(env, &out, nil)
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
	env, _ := realFixture(t, "SLACK_TOKEN=op://Private/Slack/credential\n")
	installOp(t, map[string]string{
		"account list":                       "me@example.com\n",
		"read op://Private/Slack/credential": resolved,
	})
	var out bytes.Buffer
	RunSecretCheck(env, &out)
	s := out.String()
	if strings.Contains(s, resolved) {
		t.Errorf("secret check LEAKED the resolved value:\n%s", s)
	}
	if !strings.Contains(s, "SLACK_TOKEN: OK") {
		t.Errorf("want SLACK_TOKEN OK:\n%s", s)
	}
}

// TestSecretCheckMissingRefsHintsSet: with no op-refs.env, `secret check`
// answers exit 3 ("could not check at all", never conflated with "checked, and
// a ref failed") and points at the `secret set` primitive, not the removed
// `edit`. In-process, because the code is a RETURNED error now: the subprocess
// this used to need could only ever observe a status byte.
func TestSecretCheckMissingRefsHintsSet(t *testing.T) {
	env, _ := realFixture(t, "") // no op-refs.env at all
	var out bytes.Buffer
	err := RunSecretCheck(env, &out)
	if got := cli.ExitCode(err); got != 3 {
		t.Errorf("exit code = %d, want 3 (err = %v)", got, err)
	}
	if !strings.Contains(out.String(), "pix secret set <ENV_VAR> op://vault/item/field") {
		t.Errorf("want a `secret set` hint, got:\n%s", out.String())
	}
	if err != nil && strings.Contains(err.Error(), "op-refs.env") {
		t.Errorf("the reason is printed once, on the writer, not re-rendered by the exit mapper: %v", err)
	}
}

// --- secret set / rm: hermetic, via an in-memory hostenv.Env (readFile/writeFile
// backed by a plain map — no real disk touched). ---

// memEnv builds a hostenv.Env whose readFile/writeFile operate on an in-memory
// files map. The caller sets $PIX_HOME to fakeHome (memHome below) so
// DefaultOpRefsPath — PIX_HOME-only now (QA F5) — lines up with fakeRefsPath;
// nothing here touches a real directory.
func memEnv(t *testing.T, files map[string]string) hostenv.Env {
	t.Helper()
	t.Setenv("PIX_HOME", fakeHome) // op-refs.env resolves under PIX_HOME alone (QA F5)
	return hostenv.Env{System: &systest.Fake{GetenvFn: func(string) string {
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

const fakeHome = "/fake/config"

const fakeRefsPath = fakeHome + "/op-refs.env"

func TestSecretSetUpsertsNewKey(t *testing.T) {
	files := map[string]string{fakeRefsPath: "# header\nSLACK_TOKEN=op://Private/Slack/credential\n"}
	var out bytes.Buffer
	RunSecretSet(memEnv(t, files), &out, "GITHUB_TOKEN", "op://Private/GitHub/credential", nil)
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
	RunSecretSet(memEnv(t, files), &bytes.Buffer{}, "SLACK_TOKEN", "op://Private/Slack/new", nil)
	got := files[fakeRefsPath]
	want := "# a comment\nSLACK_TOKEN=op://Private/Slack/new\n\nGOG_ACCOUNT=me@example.com\n"
	if got != want {
		t.Errorf("content after replace = %q, want %q", got, want)
	}
}

// TestSecretSetRejectsNonRefForSecretKey: a pasted value is refused (exit 2),
// the message never echoes it, and — newly assertable now the rejection is a
// returned error rather than an os.Exit — op-refs.env is left byte-identical.
func TestSecretSetRejectsNonRefForSecretKey(t *testing.T) {
	files := map[string]string{fakeRefsPath: "X=1\n"}
	var out bytes.Buffer
	err := RunSecretSet(memEnv(t, files), &out, "SLACK_TOKEN", "xoxb-pasted-secret-value", nil)
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("exit code = %d, want 2 (err = %v)", got, err)
	}
	if strings.Contains(out.String(), "xoxb-pasted-secret-value") {
		t.Errorf("rejection message LEAKED the pasted value: %s", out.String())
	}
	if !strings.Contains(out.String(), "refs-only") {
		t.Errorf("rejection message should explain the refs-only policy: %s", out.String())
	}
	if files[fakeRefsPath] != "X=1\n" {
		t.Errorf("a rejected invocation must not touch op-refs.env, got %q", files[fakeRefsPath])
	}
}

// TestSecretSetRejectsControlChars is the injection regression: a value carrying
// a newline must be refused (exit 2), never written, so it cannot smuggle a
// SECOND KEY=value line (e.g. a pasted plaintext secret) into op-refs.env. The
// "never written" half is what the old subprocess form could not check — its
// in-memory refs file died with the child.
func TestSecretSetRejectsControlChars(t *testing.T) {
	files := map[string]string{fakeRefsPath: "X=1\n"}
	var out bytes.Buffer
	err := RunSecretSet(memEnv(t, files), &out, "GITHUB_TOKEN", "op://V/I/f\nSLACK_TOKEN=xoxb-injected", nil)
	if got := cli.ExitCode(err); got != 2 {
		t.Errorf("exit code = %d, want 2 (err = %v)", got, err)
	}
	if strings.Contains(out.String(), "xoxb-injected") {
		t.Errorf("rejection LEAKED the injected value: %s", out.String())
	}
	if !strings.Contains(out.String(), "control character") {
		t.Errorf("rejection should name the control-character reason: %s", out.String())
	}
	if files[fakeRefsPath] != "X=1\n" {
		t.Errorf("the smuggled line must never reach op-refs.env, got %q", files[fakeRefsPath])
	}
}

// TestSecretSetAllowsNonSecretAllowlistLiteral: the CALLER-supplied allowlist
// (the active pack's `env_keys`) is what authorizes a plain literal. The global
// config.NonSecretOpRefsKeys map is gone, so the permission travels as an
// argument from the thing that declared it.
func TestSecretSetAllowsNonSecretAllowlistLiteral(t *testing.T) {
	files := map[string]string{fakeRefsPath: "X=1\n"}
	var out bytes.Buffer
	if err := RunSecretSet(memEnv(t, files), &out, "GOG_ACCOUNT", "me@example.com",
		NonSecret{"GOG_ACCOUNT": true}); err != nil {
		t.Fatalf("a pack-authorized literal must be accepted: %v (out=%q)", err, out.String())
	}
	if got := files[fakeRefsPath]; !strings.Contains(got, "GOG_ACCOUNT=me@example.com") {
		t.Errorf("content after set = %q, want GOG_ACCOUNT set to the literal", got)
	}
}

// TestSecretSetRejectsLiteralWithoutAllowlist is the negative half, and the
// whole point of making the allowlist a parameter: the SAME key/value pair that
// the test above accepts is REFUSED (exit 2, nothing written) when no caller
// authorizes it. A nil allowlist means "must be an op:// ref" — which is the
// correct posture for a host with no active pack.
func TestSecretSetRejectsLiteralWithoutAllowlist(t *testing.T) {
	for name, allow := range map[string]NonSecret{
		"nil allowlist":        nil,
		"other key allowed":    {"SOME_OTHER_VAR": true},
		"key explicitly false": {"GOG_ACCOUNT": false},
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{fakeRefsPath: "X=1\n"}
			var out bytes.Buffer
			err := RunSecretSet(memEnv(t, files), &out, "GOG_ACCOUNT", "me@example.com", allow)
			if got := cli.ExitCode(err); got != 2 {
				t.Errorf("exit code = %d, want 2 (err = %v)", got, err)
			}
			if !strings.Contains(out.String(), "not an op:// ref") {
				t.Errorf("rejection should explain the refs-only policy: %s", out.String())
			}
			if files[fakeRefsPath] != "X=1\n" {
				t.Errorf("an unauthorized literal must never reach op-refs.env, got %q", files[fakeRefsPath])
			}
		})
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
	RunSecretSet(memEnv(t, files), &out, "OPENAI_API_KEY", "op://Docker/OPENAI_API_KEY/api key", nil)
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
	env := memEnv(t, files)
	var out bytes.Buffer
	RunSecretSet(env, &out, "ANTHROPIC_API_KEY", "op://Vault/Item/api%20key", nil)

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
	t.Setenv("PIX_HOME", dir) // op-refs.env resolves under PIX_HOME alone (QA F5)
	// config.OpRefsPath (read below) resolves through PIX_HOME only (QA F5);
	// pointed at the SAME dir as PIX_CONFIG above so RunSecretSet's write
	// (via secret.DefaultOpRefsPath) and this test's read agree on one path.
	t.Setenv("PIX_HOME", dir)
	env := hostenv.Env{System: sys.Real{}}
	var out bytes.Buffer
	RunSecretSet(env, &out, "SLACK_TOKEN", "op://Private/Slack/credential", nil)

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
	RunSecretRm(memEnv(t, files), &out, "SLACK_TOKEN")
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
	RunSecretRm(memEnv(t, files), &out, "NOPE_NOT_THERE")
	if files[fakeRefsPath] != "SLACK_TOKEN=op://Private/Slack/credential\n" {
		t.Errorf("rm on a missing key must not modify the file, got %q", files[fakeRefsPath])
	}
	if !strings.Contains(out.String(), "no ref named NOPE_NOT_THERE") {
		t.Errorf("output = %q, want a clean no-op message", out.String())
	}
}

func TestSecretRmMissingFileIsCleanNoop(t *testing.T) {
	var out bytes.Buffer
	RunSecretRm(memEnv(t, map[string]string{}), &out, "SLACK_TOKEN")
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("output = %q, want a clear missing-file message", out.String())
	}
}

func TestSecretSetAndRmFailOnUnreadableOpRefs(t *testing.T) {
	env := memEnv(t, map[string]string{})
	systest.Of(env.System).ReadFileFn = func(string) (string, error) { return "", os.ErrPermission }

	var setOut bytes.Buffer
	if err := RunSecretSet(env, &setOut, "SLACK_TOKEN", "op://v/slack/token", nil); err == nil {
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
	env := memEnv(t, files)

	var setOut bytes.Buffer
	if err := RunSecretSet(env, &setOut, "OPENAI_API_KEY", "op://v/openai/key", nil); err != nil {
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

// TestModelKeyMissingMessage_NamesNoRemovedVerb is QA re-review F2: the
// launch-blocking "no model key" guidance used to point at "pix models add
// anthropic" and "pix secret sync", both removed v1 verbs that exit 2
// (unknown command) in v2. Every branch (refs present, refs absent) must
// name only real v2 verbs.
func TestModelKeyMissingMessage_NamesNoRemovedVerb(t *testing.T) {
	removed := []string{"pix models", "pix secret sync", "secret sync"}

	t.Run("refs absent", func(t *testing.T) {
		env := memEnv(t, map[string]string{})
		msg := ModelKeyMissingMessage(env)
		for _, r := range removed {
			if strings.Contains(msg, r) {
				t.Errorf("message names removed verb %q:\n%s", r, msg)
			}
		}
		if !strings.Contains(msg, "pix setup") || !strings.Contains(msg, "pix secret set") {
			t.Errorf("message must still guide to real v2 verbs, got:\n%s", msg)
		}
	})

	t.Run("refs present", func(t *testing.T) {
		env := memEnv(t, map[string]string{fakeRefsPath: "ANTHROPIC_API_KEY=op://v/anthropic/key\n"})
		msg := ModelKeyMissingMessage(env)
		for _, r := range removed {
			if strings.Contains(msg, r) {
				t.Errorf("message names removed verb %q:\n%s", r, msg)
			}
		}
		if !strings.Contains(msg, "pix secret check") {
			t.Errorf("message with refs present must point at `pix secret check`, got:\n%s", msg)
		}
	})
}
