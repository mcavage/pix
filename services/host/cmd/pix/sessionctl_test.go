package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestHiddenSessionVerbsAbsentFromHelp is the explicit ask: the internal
// invocation modes must never render in `pix help --all`, and must never
// register as a dispatchable kong verb (knownVerbs, the did-you-mean
// suggester's vocabulary).
func TestHiddenSessionVerbsAbsentFromHelp(t *testing.T) {
	all := helpAll()
	for _, hidden := range []string{hiddenSessionMCPVerb, hiddenSessionChildVerb, "session-mcp", "session-child"} {
		if strings.Contains(all, hidden) {
			t.Fatalf("helpAll() must never mention %q, got:\n%s", hidden, all)
		}
	}
	verbs := knownVerbs()
	if verbs[hiddenSessionMCPVerb] || verbs[hiddenSessionChildVerb] {
		t.Fatalf("hidden session verbs must not be knownVerbs(): %v", verbs)
	}
}

// TestDispatchInterceptsHiddenVerbsBeforeKong proves the hidden verbs never
// reach classifyBareArg/kong: an unset PIX_SESSION_* environment still
// produces a clean, non-panicking exit (a refusal, not a crash), which
// would not be true if this fell through into the bare-arg/verb-typo path
// (a token starting with "__" is not a directory and not a known verb, so
// it would otherwise print "no command named" and exit 2 WITHOUT ever
// calling runHiddenSessionVerb).
func TestDispatchInterceptsHiddenVerbsBeforeKong(t *testing.T) {
	var out, errBuf bytes.Buffer
	d := &cliDeps{Out: &out, Err: &errBuf}
	code, handled := runHiddenSessionVerb([]string{hiddenSessionChildVerb}, d)
	if !handled {
		t.Fatal("runHiddenSessionVerb did not claim the child verb")
	}
	if code == 0 {
		t.Fatal("a session-child invocation with no flags at all must not report success")
	}
	if strings.Contains(errBuf.String(), "no command named") {
		t.Fatalf("hidden verb fell through to the verb-typo path: %s", errBuf.String())
	}
}

func TestRunHiddenSessionVerb_IgnoresOrdinaryVerbs(t *testing.T) {
	d := &cliDeps{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	if _, handled := runHiddenSessionVerb([]string{"run"}, d); handled {
		t.Fatal("an ordinary verb must never be claimed by the hidden session dispatch")
	}
	if _, handled := runHiddenSessionVerb(nil, d); handled {
		t.Fatal("empty argv must never be claimed")
	}
}

func TestSessionContextFromEnv_NamesEveryMissingVariable(t *testing.T) {
	t.Setenv(envSessionTree, "")
	t.Setenv(envSessionSandbox, "")
	t.Setenv(envSessionInstance, "")
	t.Setenv(envSessionDir, "")
	t.Setenv(envSessionStore, "")
	_, err := sessionContextFromEnv()
	if err == nil {
		t.Fatal("expected an error when the session env is entirely unset")
	}
	for _, name := range []string{envSessionTree, envSessionSandbox, envSessionInstance, envSessionDir, envSessionStore} {
		if !strings.Contains(err.Error(), name) {
			t.Fatalf("error %q does not name missing %s", err, name)
		}
	}
}

func TestRunSessionChild_RefusesIncompleteRequest(t *testing.T) {
	d := &cliDeps{Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	code := runSessionChild(nil, d)
	if code == 0 {
		t.Fatal("an empty session-child invocation must not report success")
	}
}
