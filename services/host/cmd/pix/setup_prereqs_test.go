package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"pix/host/hostenv"
	"pix/host/sys/systest"
	"pix/host/workflow/doctor"
	"pix/host/workflow/setup"
	"strings"
	"testing"
)

func TestEnsureSetupPrereqsInstallsCoreToolsOnce(t *testing.T) {
	old := setup.SetupHostOS
	setup.SetupHostOS = "darwin"
	t.Cleanup(func() { setup.SetupHostOS = old })
	installed := map[string]bool{"brew": true}
	runs := 0
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if installed[name] {
			return "/opt/homebrew/bin/" + name, nil
		}
		return "", fmt.Errorf("missing")
	}, RunInteractiveFn: func(name string, args ...string) error {
		runs++
		if name != "brew" || strings.Join(args, " ") != "install docker/tap/sbx@nightly 1password-cli" {
			t.Fatalf("unexpected install: %s %v", name, args)
		}
		installed["sbx"], installed["op"] = true, true
		return nil
	}}}
	var out bytes.Buffer
	if err := setup.EnsureSetupPrereqs(env, strings.NewReader("\n"), &out, true); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || !strings.Contains(out.String(), "[Y/n]") {
		t.Fatalf("runs=%d output=%q", runs, out.String())
	}
}

func TestEnsureSetupPrereqsNonInteractivePrintsExactFixes(t *testing.T) {
	old := setup.SetupHostOS
	setup.SetupHostOS = "darwin"
	t.Cleanup(func() { setup.SetupHostOS = old })
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "brew" {
			return "/opt/homebrew/bin/brew", nil
		}
		return "", fmt.Errorf("missing")
	}}}
	err := setup.EnsureSetupPrereqs(env, nil, &bytes.Buffer{}, false)
	if err == nil || !strings.Contains(err.Error(), doctor.SbxInstallHint) || !strings.Contains(err.Error(), "brew install 1password-cli") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureSetupPrereqsIgnoresOptionalTools(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" || name == "op" {
			return "/bin/" + name, nil
		}
		return "", fmt.Errorf("missing")
	}}}
	if err := setup.EnsureSetupPrereqs(env, nil, &bytes.Buffer{}, false); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSetupSbxSessionRunsOfficialLoginAndVerifies(t *testing.T) {
	ready := false
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(name string, args ...string) (string, bool, error) {
		if ready {
			return "", false, nil
		}
		return "", false, fmt.Errorf("not signed in")
	}, RunInteractiveFn: func(name string, args ...string) error {
		if name != "sbx" || strings.Join(args, " ") != "login" {
			t.Fatalf("unexpected login: %s %v", name, args)
		}
		ready = true
		return nil
	}}}
	var out bytes.Buffer
	if err := setup.EnsureSetupSbxSession(env, &out, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "official `sbx login` flow") {
		t.Fatalf("missing handoff: %s", out.String())
	}
}

func TestEnsureSetupSbxSessionNonInteractivePrintsFix(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return "", false, fmt.Errorf("not signed in")
	}}}
	if err := setup.EnsureSetupSbxSession(env, &bytes.Buffer{}, false); err == nil || !strings.Contains(err.Error(), "sbx login") {
		t.Fatalf("got %v", err)
	}
}

func TestEnsureSetupSbxDefaultsAddsPublisherAndInitializesOpenPolicy(t *testing.T) {
	sources := []string{"docker.io/", "github.com/example/"}
	policyInitialized := false
	var mutations []string
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(name string, args ...string) (string, bool, error) {
		got := strings.Join(append([]string{name}, args...), " ")
		switch got {
		case "sbx settings get kit.allowedSources":
			b, _ := json.Marshal(sources)
			return string(b), false, nil
		case "sbx policy ls --source local --type network --json":
			if policyInitialized {
				return `[{"name":"global"}]`, false, nil
			}
			return `[]`, false, nil
		default:
			return "", false, fmt.Errorf("unexpected probe: %s", got)
		}
	}, RunInteractiveQuietFn: func(name string, args ...string) error {
		got := strings.Join(append([]string{name}, args...), " ")
		mutations = append(mutations, got)
		switch {
		case len(args) == 4 && strings.Join(args[:3], " ") == "settings set kit.allowedSources":
			return json.Unmarshal([]byte(args[3]), &sources)
		case got == "sbx policy init allow-all":
			policyInitialized = true
			return nil
		default:
			return fmt.Errorf("unexpected mutation: %s", got)
		}
	}}}
	if err := setup.EnsureSetupSbxDefaults(env); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sources, ","); got != "docker.io/,github.com/example/,github.com/mcavage/" {
		t.Fatalf("sources=%q", got)
	}
	if !policyInitialized {
		t.Fatal("network policy was not initialized")
	}
	if len(mutations) != 2 {
		t.Fatalf("mutations=%v", mutations)
	}
}

func TestEnsureSetupSbxDefaultsPreservesWildcardAndExistingPolicy(t *testing.T) {
	mutations := 0
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(name string, args ...string) (string, bool, error) {
		switch strings.Join(append([]string{name}, args...), " ") {
		case "sbx settings get kit.allowedSources":
			return `["*"]`, false, nil
		case "sbx policy ls --source local --type network --json":
			return `{"rules":[{"decision":"deny"}]}`, false, nil
		default:
			return "", false, fmt.Errorf("unexpected probe")
		}
	}, RunInteractiveQuietFn: func(string, ...string) error {
		mutations++
		return nil
	}}}
	if err := setup.EnsureSetupSbxDefaults(env); err != nil {
		t.Fatal(err)
	}
	if mutations != 0 {
		t.Fatalf("existing sbx policy was mutated %d time(s)", mutations)
	}
}

func TestSetupSbxNetworkPolicyInitializedAcceptsLegacyArray(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `[{"decision":"allow"}]`, false, nil
	}}}
	initialized, err := setup.SetupSbxNetworkPolicyInitialized(env)
	if err != nil || !initialized {
		t.Fatalf("initialized=%v err=%v", initialized, err)
	}
}

func TestSetupSbxNetworkPolicyInitializedAcceptsEmptyRulesObject(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `{"rules":[]}`, false, nil
	}}}
	initialized, err := setup.SetupSbxNetworkPolicyInitialized(env)
	if err != nil || initialized {
		t.Fatalf("initialized=%v err=%v", initialized, err)
	}
}

func TestEnsureSetupOpenNetworkPolicyHandlesUninitializedListError(t *testing.T) {
	initialized := false
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		if !initialized {
			return "", false, fmt.Errorf("network policy is not initialized")
		}
		return `[{"name":"global"}]`, false, nil
	}, RunInteractiveQuietFn: func(name string, args ...string) error {
		if name != "sbx" || strings.Join(args, " ") != "policy init allow-all" {
			return fmt.Errorf("unexpected command: %s %v", name, args)
		}
		initialized = true
		return nil
	}}}
	if err := setup.EnsureSetupOpenNetworkPolicy(env); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureSetupKitAllowedSourceRequiresVerifiedPersistence(t *testing.T) {
	reads := 0
	env := hostenv.Env{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		reads++
		return `["docker.io/"]`, false, nil
	}, RunInteractiveQuietFn: func(string, ...string) error { return nil }}}
	err := setup.EnsureSetupKitAllowedSource(env)
	if err == nil || !strings.Contains(err.Error(), "did not retain") || reads != 2 {
		t.Fatalf("err=%v reads=%d", err, reads)
	}
}
