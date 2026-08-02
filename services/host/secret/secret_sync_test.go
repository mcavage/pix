package secret

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

// fakeSyncEnv builds a hostenv.Env whose op-refs.env content is fixed, op is
// installed+signed-in, sbx is present, and op read returns a canned value.
func fakeSyncEnv(refs string, opReadVal string, sbxSetErr error, capture *[]string) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return refs, nil }, LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if capture != nil {
			*capture = append(*capture, name+" "+strings.Join(args, " "))
		}
		switch {
		case name == "op" && len(args) >= 1 && args[0] == "--version":
			return "2.0", nil
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil // OpSignedIn
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return opReadVal, nil
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "set":
			return "", sbxSetErr
		}
		return "", nil
	}}}
}

func TestSyncProviderKeys_Success(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://Private/anthropic/key\nGEMINI_API_KEY=op://Private/gemini/key\n"
	var calls []string
	env := fakeSyncEnv(refs, "sk-secret-value\n", nil, &calls)
	var out bytes.Buffer
	synced, failed, fatal := syncProviderKeys(env, &out)
	if fatal != nil || failed != 0 || synced != 2 {
		t.Fatalf("synced=%d failed=%d fatal=%v; out=%q", synced, failed, fatal, out.String())
	}
	// The resolved value must NEVER be printed.
	if strings.Contains(out.String(), "sk-secret-value") {
		t.Error("resolved secret value leaked into output")
	}
	// It must map ENV var -> sbx secret name (anthropic, google) and pass the value to sbx.
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "sbx secret set -f -g anthropic -t sk-secret-value") ||
		!strings.Contains(joined, "sbx secret set -f -g google -t sk-secret-value") {
		t.Errorf("expected sbx secret set for anthropic+google, got:\n%s", joined)
	}
}

// A duplicate ANTHROPIC_API_KEY line must resolve the FIRST conflicting ref
// — never whichever duplicate happens to land last in the map — matching
// CurrentOpRef/mirrorProviderRefsToHostModeLocked's first-wins semantics
// (see TestMirrorProviderRefsToHostModeUsesFirstConflictingRef).
func TestSyncProviderKeysUsesFirstConflictingRef(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://vault/new/key\nANTHROPIC_API_KEY=op://vault/old/key\n"
	var calls []string
	env := fakeSyncEnv(refs, "sk-secret-value\n", nil, &calls)
	var out bytes.Buffer
	synced, failed, fatal := syncProviderKeys(env, &out)
	if fatal != nil || failed != 0 || synced != 1 {
		t.Fatalf("synced=%d failed=%d fatal=%v; out=%q", synced, failed, fatal, out.String())
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "op read op://vault/new/key") {
		t.Errorf("expected op read of the FIRST conflicting ref, got:\n%s", joined)
	}
	if strings.Contains(joined, "op read op://vault/old/key") {
		t.Errorf("must not read the second/duplicate conflicting ref, got:\n%s", joined)
	}
}

func TestSyncProviderKeys_OpMissing(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "ANTHROPIC_API_KEY=op://a/b/c\n", nil }, LookPathFn: func(name string) (string, error) { return "", fmt.Errorf("not found") }}}
	var out bytes.Buffer
	_, _, fatal := syncProviderKeys(env, &out)
	if fatal == nil {
		t.Fatal("op missing should be a fatal precondition error")
	}
}

func TestProviderKeyRefsPresent(t *testing.T) {
	// filled provider ref -> present
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "ANTHROPIC_API_KEY=op://a/b/c\n", nil }}}
	if !ProviderKeyRefsPresent(env) {
		t.Error("filled anthropic ref should be present")
	}
	// only a non-provider ref -> not present
	env2 := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "SLACK_TOKEN=op://a/b/c\n", nil }}}
	if ProviderKeyRefsPresent(env2) {
		t.Error("non-provider ref must not count as a provider key")
	}
}

// EnsureProviderKeysFromRefs must resolve ONLY keys missing from sbx, and must
// never call `op read` for a key sbx already has (no prompt on later launches).
func TestEnsureProviderKeysFromRefs_OnlyMissing(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://P/anthropic/key\nGEMINI_API_KEY=op://P/gemini/key\n"
	var calls []string
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return refs, nil }, LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls":
			return "anthropic\n", nil // anthropic already present; gemini/google missing
		case name == "op" && len(args) >= 1 && args[0] == "--version":
			return "2.0", nil
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return "val\n", nil
		}
		return "", nil
	}}}
	var out bytes.Buffer
	EnsureProviderKeysFromRefs(env, &out)
	joined := strings.Join(calls, "\n")
	// gemini -> google should be resolved and set.
	if !strings.Contains(joined, "sbx secret set -f -g google -t val") {
		t.Errorf("missing key (google) should have been resolved:\n%s", joined)
	}
	// anthropic is already in sbx: it must NOT be op-read (no prompt).
	if strings.Contains(joined, "op read op://P/anthropic/key") {
		t.Error("must not op-read a key that sbx already has")
	}
}

// A duplicate GEMINI_API_KEY line must resolve the FIRST conflicting ref, same
// as syncProviderKeys and CurrentOpRef/mirror.
func TestEnsureProviderKeysFromRefsUsesFirstConflictingRef(t *testing.T) {
	refs := "GEMINI_API_KEY=op://vault/new/key\nGEMINI_API_KEY=op://vault/old/key\n"
	var calls []string
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return refs, nil }, LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls":
			return "", nil // google not present yet
		case name == "op" && len(args) >= 1 && args[0] == "--version":
			return "2.0", nil
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return "val\n", nil
		}
		return "", nil
	}}}
	var out bytes.Buffer
	EnsureProviderKeysFromRefs(env, &out)
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "op read op://vault/new/key") {
		t.Errorf("expected op read of the FIRST conflicting ref, got:\n%s", joined)
	}
	if strings.Contains(joined, "op read op://vault/old/key") {
		t.Errorf("must not read the second/duplicate conflicting ref, got:\n%s", joined)
	}
	if !strings.Contains(joined, "sbx secret set -f -g google -t val") {
		t.Errorf("expected google resolved from the first conflicting ref, got:\n%s", joined)
	}
}

// OfferOnePasswordKeys must stay silent unless TTY + op installed + no refs yet.
func TestOfferOnePasswordKeys_Gating(t *testing.T) {
	opEnv := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "op" && len(args) >= 1 && args[0] == "--version" {
			return "2.0", nil
		}
		return "", nil
	}, ReadFileFn: func(string) (string, error) { return "", nil }}}
	// not a tty -> silent
	var out bytes.Buffer
	OfferOnePasswordKeys(opEnv, strings.NewReader("y\n"), &out, false)
	if out.String() != "" {
		t.Errorf("must be silent when not a tty, got %q", out.String())
	}
	// refs already present -> silent even on a tty
	out.Reset()
	withRefs := opEnv
	systest.Of(withRefs.System).ReadFileFn = func(string) (string, error) { return "ANTHROPIC_API_KEY=op://a/b/c\n", nil }
	OfferOnePasswordKeys(withRefs, strings.NewReader("y\n"), &out, true)
	if strings.Contains(out.String(), "1Password") {
		t.Errorf("must not offer when key refs already exist, got %q", out.String())
	}
	// op not installed -> silent
	out.Reset()
	noOp := opEnv
	systest.Of(noOp.System).LookPathFn = func(string) (string, error) { return "", fmt.Errorf("nope") }
	OfferOnePasswordKeys(noOp, strings.NewReader("y\n"), &out, true)
	if out.String() != "" {
		t.Errorf("must be silent when op is not installed, got %q", out.String())
	}
}

func TestNormalizeOpRef(t *testing.T) {
	cases := map[string]string{
		`"op://Docker/ANTHROPIC_API_KEY/credential"`: "op://Docker/ANTHROPIC_API_KEY/credential",
		`  op://V/I/f  `: "op://V/I/f",
		`'op://V/I/f'`:   "op://V/I/f",
		`op://V/I/f`:     "op://V/I/f",
		`"op://V/I/f`:    `"op://V/I/f`, // unbalanced: left as-is
	}
	for in, want := range cases {
		if got := NormalizeOpRef(in); got != want {
			t.Errorf("NormalizeOpRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// mirrorProviderRefsToHostMode copies FILLED provider refs from op-refs.env into
// hostmode.env, upserting without touching unrelated entries.
func TestMirrorProviderRefsToHostMode(t *testing.T) {
	files := map[string]string{}
	dir := "/cfg/pix"
	files[filepath.Join(dir, "op-refs.env")] = "ANTHROPIC_API_KEY=op://v/anthropic/key\nSLACK_TOKEN=op://v/slack/tok\n"
	files[filepath.Join(dir, "hostmode.env")] = "EXISTING=op://v/x/y\n"
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/cfg"
		}
		return ""
	}, ReadFileFn: func(p string) (string, error) {
		if v, ok := files[p]; ok {
			return v, nil
		}
		return "", os.ErrNotExist
	}, WriteFileFn: func(p string, d []byte, _ os.FileMode) error { files[p] = string(d); return nil }}}
	mirrorProviderRefsToHostMode(env)
	got := files[filepath.Join(dir, "hostmode.env")]
	if !strings.Contains(got, "ANTHROPIC_API_KEY=op://v/anthropic/key") {
		t.Errorf("provider ref not mirrored into hostmode.env: %q", got)
	}
	if !strings.Contains(got, "EXISTING=op://v/x/y") {
		t.Errorf("mirror clobbered an unrelated hostmode.env entry: %q", got)
	}
	if strings.Contains(got, "SLACK_TOKEN") {
		t.Errorf("mirror copied a non-provider ref: %q", got)
	}
}

func TestMirrorProviderRefsToHostModeUsesFirstConflictingRef(t *testing.T) {
	files := map[string]string{}
	dir := "/cfg/pix"
	files[filepath.Join(dir, "op-refs.env")] = "ANTHROPIC_API_KEY=op://vault/new/key\nANTHROPIC_API_KEY=op://vault/old/key\n"
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/cfg"
		}
		return ""
	}, ReadFileFn: func(p string) (string, error) {
		if v, ok := files[p]; ok {
			return v, nil
		}
		return "", os.ErrNotExist
	}, WriteFileFn: func(p string, d []byte, _ os.FileMode) error { files[p] = string(d); return nil }}}
	mirrorProviderRefsToHostMode(env)
	got := files[filepath.Join(dir, "hostmode.env")]
	if !strings.Contains(got, "ANTHROPIC_API_KEY=op://vault/new/key") || strings.Contains(got, "old/key") {
		t.Fatalf("mirror must use the same first ref setup validates, got %q", got)
	}
}

// writeOpRefFileQuiet must NOT clobber a file it can't read (a real read error,
// e.g. EACCES); it fails closed instead of truncating to a single entry.
func TestWriteOpRefFileQuiet_ReadErrorNoClobber(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "", errors.New("permission denied") }, WriteFileFn: func(string, []byte, os.FileMode) error { t.Fatal("must not write when read fails"); return nil }}}
	if err := writeOpRefFileQuiet(env, "/x/op-refs.env", "ANTHROPIC_API_KEY", "op://v/a/k"); err == nil {
		t.Fatal("expected error on unreadable file, got nil")
	}
}

// writeOpRefFileQuiet stores refs with LITERAL spaces (op 2.35.0: both `op read`
// and `op run --env-file` require literal spaces and reject %20), and decodes any
// %20 from refs written by the old buggy encoding so files self-heal.
func TestWriteOpRefFileQuiet_KeepsLiteralSpaces_DecodesEncoded(t *testing.T) {
	write := func(val string) string {
		var written string
		env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "", os.ErrNotExist }, WriteFileFn: func(_ string, d []byte, _ os.FileMode) error { written = string(d); return nil }}}
		if err := writeOpRefFileQuiet(env, "/x/hostmode.env", "OPENAI_API_KEY", val); err != nil {
			t.Fatal(err)
		}
		return written
	}
	// A literal-space ref is stored literally (never encoded).
	written := write("op://Docker/OPENAI_API_KEY/api key")
	if !strings.Contains(written, "op://Docker/OPENAI_API_KEY/api key") {
		t.Errorf("literal space not preserved: %q", written)
	}
	if strings.Contains(written, "api%20key") {
		t.Errorf("space must NOT be percent-encoded (op read/op run reject %%20): %q", written)
	}
	// An already-encoded ref (from the old bug) self-heals to literal on write.
	healed := write("op://Docker/OPENAI_API_KEY/api%20key")
	if !strings.Contains(healed, "op://Docker/OPENAI_API_KEY/api key") {
		t.Errorf("existing %%20 not decoded to a literal space: %q", healed)
	}
}

// --- item 3: HostModeProviderKeys dedupes; completeness is exact-set -------

// A duplicate ANTHROPIC_API_KEY line (or any repeated provider ref) must
// never inflate HostModeProviderKeys past the real distinct-provider count —
// dedupe is by PROVIDER NAME, not by input line.
func TestHostModeProviderKeys_DedupesDuplicateEntries(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{GetenvFn: func(k string) string {
		if k == "XDG_CONFIG_HOME" {
			return "/cfg"
		}
		return ""
	}, ReadFileFn: func(p string) (string, error) {
		if p == filepath.Join("/cfg", "pix", "hostmode.env") {
			// ANTHROPIC_API_KEY declared TWICE, GEMINI_API_KEY never.
			return "ANTHROPIC_API_KEY=op://v/a/k\nANTHROPIC_API_KEY=op://v/a/k2\nOPENAI_API_KEY=op://v/o/k\n", nil
		}
		return "", os.ErrNotExist
	}}}
	got, err := HostModeProviderKeys(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("duplicate anthropic entries must not inflate the count: got %v (len %d), want 2 distinct providers", got, len(got))
	}
	if hasAllProviderKeyNames(got) {
		t.Error("a duplicate anthropic entry must NEVER let the exact-set check pass with google actually missing")
	}
}

// hasAllProviderKeyNames is a set-membership check, not a length check: a
// padded-but-incomplete list (e.g. from a duplicate/aliased entry) must not
// satisfy it, and the real three-provider set must.
func TestHasAllProviderKeyNames_ExactSetNotLength(t *testing.T) {
	if hasAllProviderKeyNames([]string{"anthropic", "anthropic", "openai"}) {
		t.Error("a duplicate name padding the length to 3 must NOT count as complete (google is missing)")
	}
	if !hasAllProviderKeyNames([]string{"anthropic", "openai", "google"}) {
		t.Error("the real three-provider set must count as complete")
	}
	if hasAllProviderKeyNames([]string{"anthropic", "openai"}) {
		t.Error("two of three must not count as complete")
	}
}

// --- item 4: hasRef branch writes to BOTH files, fails if either fails ----

// --- item 2: OpReadNonEmpty trims all whitespace, rejects whitespace-only --

// OpReadNonEmpty must strip leading/trailing whitespace of every kind (tabs,
// spaces, CRLF), and a value that is whitespace-only after trimming must be
// rejected exactly like a truly empty one — never accepted as a valid (if
// odd) secret.
func TestOpReadNonEmpty_TrimsWhitespaceAndRejectsWhitespaceOnly(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantVal string
		wantOK  bool
	}{
		{"tabs and spaces around a real value", "\t  sk-real-value  \t\n", "sk-real-value", true},
		{"whitespace only: spaces", "   ", "", false},
		{"whitespace only: tabs", "\t\t\t", "", false},
		{"whitespace only: mixed tabs/spaces/newlines", " \t\n \t ", "", false},
		{"truly empty", "", "", false},
		{"CRLF around a real value", "\r\nsk-real-value\r\n", "sk-real-value", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := hostenv.Env{System: &systest.Fake{RunFn: func(string, ...string) (string, error) { return c.raw, nil }}}
			val, ok := OpReadNonEmpty(env, "op://v/a/k")
			if ok != c.wantOK || val != c.wantVal {
				t.Errorf("OpReadNonEmpty(%q) = (%q, %v), want (%q, %v)", c.raw, val, ok, c.wantVal, c.wantOK)
			}
		})
	}
}

// --- item 3: syncProviderKeys redacts resolved values from output+error ---

// syncProviderKeys must redact the resolved value from BOTH sbx's own raw
// output and the wrapping Go error text before printing either — mirrors
// TestSyncProviderKeyToSbx_RedactsValueFromOutputAndError but for the
// legacy/general sync path.
func TestSyncProviderKeys_RedactsValueFromOutputAndError(t *testing.T) {
	const secretVal = "sk-should-never-print-either"
	refs := "ANTHROPIC_API_KEY=op://Private/anthropic/key\n"
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return refs, nil }, LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		switch {
		case name == "op" && len(args) >= 1 && args[0] == "--version":
			return "2.0", nil
		case name == "op" && len(args) >= 1 && args[0] == "account":
			return "acct", nil
		case name == "op" && len(args) >= 1 && args[0] == "read":
			return secretVal, nil
		case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "set":
			// Simulate sbx echoing the full failed command (including the
			// secret value) back in its own stdout/stderr AND the wrapping Go
			// error carrying the same argv.
			return "sbx: command failed: sbx secret set -f -g anthropic -t " + secretVal,
				fmt.Errorf("exit status 1: -t %s", secretVal)
		}
		return "", nil
	}}}
	var out bytes.Buffer
	synced, failed, fatal := syncProviderKeys(env, &out)
	if fatal != nil || synced != 0 || failed != 1 {
		t.Fatalf("synced=%d failed=%d fatal=%v; out=%q", synced, failed, fatal, out.String())
	}
	if strings.Contains(out.String(), secretVal) {
		t.Errorf("resolved secret value must never appear in printed output, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "***") {
		t.Errorf("expected the redaction marker in place of the value, got:\n%s", out.String())
	}
}
