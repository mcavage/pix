package main

import (
	"bytes"
	"fmt"
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
