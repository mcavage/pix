package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSyncEnv builds a shellEnv whose op-refs.env content is fixed, op is
// installed+signed-in, sbx is present, and op read returns a canned value.
func fakeSyncEnv(refs string, opReadVal string, sbxSetErr error, capture *[]string) shellEnv {
	return shellEnv{
		readFile: func(string) (string, error) { return refs, nil },
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if capture != nil {
				*capture = append(*capture, name+" "+strings.Join(args, " "))
			}
			switch {
			case name == "op" && len(args) >= 1 && args[0] == "--version":
				return "2.0", nil
			case name == "op" && len(args) >= 1 && args[0] == "account":
				return "acct", nil // opSignedIn
			case name == "op" && len(args) >= 1 && args[0] == "read":
				return opReadVal, nil
			case name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "set":
				return "", sbxSetErr
			}
			return "", nil
		},
	}
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
	if !strings.Contains(joined, "sbx secret set -g anthropic -t sk-secret-value") ||
		!strings.Contains(joined, "sbx secret set -g google -t sk-secret-value") {
		t.Errorf("expected sbx secret set for anthropic+google, got:\n%s", joined)
	}
}

func TestSyncProviderKeys_OpMissing(t *testing.T) {
	env := shellEnv{
		readFile: func(string) (string, error) { return "ANTHROPIC_API_KEY=op://a/b/c\n", nil },
		lookPath: func(name string) (string, error) { return "", fmt.Errorf("not found") },
	}
	var out bytes.Buffer
	_, _, fatal := syncProviderKeys(env, &out)
	if fatal == nil {
		t.Fatal("op missing should be a fatal precondition error")
	}
}

func TestProviderKeyRefsPresent(t *testing.T) {
	// filled provider ref -> present
	env := shellEnv{readFile: func(string) (string, error) { return "ANTHROPIC_API_KEY=op://a/b/c\n", nil }}
	if !providerKeyRefsPresent(env) {
		t.Error("filled anthropic ref should be present")
	}
	// only a non-provider ref -> not present
	env2 := shellEnv{readFile: func(string) (string, error) { return "SLACK_TOKEN=op://a/b/c\n", nil }}
	if providerKeyRefsPresent(env2) {
		t.Error("non-provider ref must not count as a provider key")
	}
}

// ensureProviderKeysFromRefs must resolve ONLY keys missing from sbx, and must
// never call `op read` for a key sbx already has (no prompt on later launches).
func TestEnsureProviderKeysFromRefs_OnlyMissing(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://P/anthropic/key\nGEMINI_API_KEY=op://P/gemini/key\n"
	var calls []string
	env := shellEnv{
		readFile: func(string) (string, error) { return refs, nil },
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
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
		},
	}
	var out bytes.Buffer
	ensureProviderKeysFromRefs(env, &out)
	joined := strings.Join(calls, "\n")
	// gemini -> google should be resolved and set.
	if !strings.Contains(joined, "sbx secret set -g google -t val") {
		t.Errorf("missing key (google) should have been resolved:\n%s", joined)
	}
	// anthropic is already in sbx: it must NOT be op-read (no prompt).
	if strings.Contains(joined, "op read op://P/anthropic/key") {
		t.Error("must not op-read a key that sbx already has")
	}
}

// offerOnePasswordKeys must stay silent unless TTY + op installed + no refs yet.
func TestOfferOnePasswordKeys_Gating(t *testing.T) {
	opEnv := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "op" && len(args) >= 1 && args[0] == "--version" {
				return "2.0", nil
			}
			return "", nil
		},
		readFile: func(string) (string, error) { return "", nil }, // no refs
	}
	// not a tty -> silent
	var out bytes.Buffer
	offerOnePasswordKeys(opEnv, strings.NewReader("y\n"), &out, false)
	if out.String() != "" {
		t.Errorf("must be silent when not a tty, got %q", out.String())
	}
	// refs already present -> silent even on a tty
	out.Reset()
	withRefs := opEnv
	withRefs.readFile = func(string) (string, error) { return "ANTHROPIC_API_KEY=op://a/b/c\n", nil }
	offerOnePasswordKeys(withRefs, strings.NewReader("y\n"), &out, true)
	if strings.Contains(out.String(), "1Password") {
		t.Errorf("must not offer when key refs already exist, got %q", out.String())
	}
	// op not installed -> silent
	out.Reset()
	noOp := opEnv
	noOp.lookPath = func(string) (string, error) { return "", fmt.Errorf("nope") }
	offerOnePasswordKeys(noOp, strings.NewReader("y\n"), &out, true)
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
		if got := normalizeOpRef(in); got != want {
			t.Errorf("normalizeOpRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// mirrorProviderRefsToHostMode copies FILLED provider refs from op-refs.env into
// hostmode.env, upserting without touching unrelated entries.
func TestMirrorProviderRefsToHostMode(t *testing.T) {
	files := map[string]string{}
	dir := "/cfg/pi-stack"
	files[filepath.Join(dir, "op-refs.env")] = "ANTHROPIC_API_KEY=op://v/anthropic/key\nSLACK_TOKEN=op://v/slack/tok\n"
	files[filepath.Join(dir, "hostmode.env")] = "EXISTING=op://v/x/y\n"
	env := shellEnv{
		getenv: func(k string) string {
			if k == "XDG_CONFIG_HOME" {
				return "/cfg"
			}
			return ""
		},
		readFile: func(p string) (string, error) {
			if v, ok := files[p]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		},
		writeFile: func(p string, d []byte, _ os.FileMode) error { files[p] = string(d); return nil },
	}
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

// writeOpRefFileQuiet must NOT clobber a file it can't read (a real read error,
// e.g. EACCES); it fails closed instead of truncating to a single entry.
func TestWriteOpRefFileQuiet_ReadErrorNoClobber(t *testing.T) {
	env := shellEnv{
		readFile:  func(string) (string, error) { return "", errors.New("permission denied") },
		writeFile: func(string, []byte, os.FileMode) error { t.Fatal("must not write when read fails"); return nil },
	}
	if err := writeOpRefFileQuiet(env, "/x/op-refs.env", "ANTHROPIC_API_KEY", "op://v/a/k"); err == nil {
		t.Fatal("expected error on unreadable file, got nil")
	}
}
