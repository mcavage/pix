package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestEnsureSetupPrereqsInstallsCoreToolsOnce(t *testing.T) {
	old := setupHostOS
	setupHostOS = "darwin"
	t.Cleanup(func() { setupHostOS = old })
	installed := map[string]bool{"brew": true}
	runs := 0
	env := shellEnv{
		lookPath: func(name string) (string, error) {
			if installed[name] {
				return "/opt/homebrew/bin/" + name, nil
			}
			return "", fmt.Errorf("missing")
		},
		runInteractive: func(name string, args ...string) error {
			runs++
			if name != "brew" || strings.Join(args, " ") != "install docker/tap/sbx@nightly 1password-cli" {
				t.Fatalf("unexpected install: %s %v", name, args)
			}
			installed["sbx"], installed["op"] = true, true
			return nil
		},
	}
	var out bytes.Buffer
	if err := ensureSetupPrereqs(env, strings.NewReader("\n"), &out, true); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || !strings.Contains(out.String(), "[Y/n]") {
		t.Fatalf("runs=%d output=%q", runs, out.String())
	}
}

func TestEnsureSetupPrereqsNonInteractivePrintsExactFixes(t *testing.T) {
	old := setupHostOS
	setupHostOS = "darwin"
	t.Cleanup(func() { setupHostOS = old })
	env := shellEnv{lookPath: func(name string) (string, error) {
		if name == "brew" {
			return "/opt/homebrew/bin/brew", nil
		}
		return "", fmt.Errorf("missing")
	}}
	err := ensureSetupPrereqs(env, nil, &bytes.Buffer{}, false)
	if err == nil || !strings.Contains(err.Error(), sbxInstallHint) || !strings.Contains(err.Error(), "brew install 1password-cli") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureSetupPrereqsIgnoresOptionalTools(t *testing.T) {
	env := shellEnv{lookPath: func(name string) (string, error) {
		if name == "sbx" || name == "op" {
			return "/bin/" + name, nil
		}
		return "", fmt.Errorf("missing")
	}}
	if err := ensureSetupPrereqs(env, nil, &bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSetupSbxSessionRunsOfficialLoginAndVerifies(t *testing.T) {
	ready := false
	env := shellEnv{
		probe: func(name string, args ...string) (string, bool, error) {
			if ready {
				return "", false, nil
			}
			return "", false, fmt.Errorf("not signed in")
		},
		runInteractive: func(name string, args ...string) error {
			if name != "sbx" || strings.Join(args, " ") != "login" {
				t.Fatalf("unexpected login: %s %v", name, args)
			}
			ready = true
			return nil
		},
	}
	var out bytes.Buffer
	if err := ensureSetupSbxSession(env, &out, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "official `sbx login` flow") {
		t.Fatalf("missing handoff: %s", out.String())
	}
}

func TestEnsureSetupSbxSessionNonInteractivePrintsFix(t *testing.T) {
	env := shellEnv{probe: func(string, ...string) (string, bool, error) {
		return "", false, fmt.Errorf("not signed in")
	}}
	if err := ensureSetupSbxSession(env, &bytes.Buffer{}, false); err == nil || !strings.Contains(err.Error(), "sbx login") {
		t.Fatalf("got %v", err)
	}
}
