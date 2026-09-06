package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/sys"
	"pix/host/sys/systest"
)

func TestSetupDefaultOffersAllConnectionsBeforeOllamaChoice(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	dir, _ := fakeInstallDir(t, "2.0.0")
	env := ollamaEnvAt(t, "", `{"models":[{"name":"qwen3.5:9b"},{"name":"nomic-embed-text"}]}`)
	fake := env.System.(*systest.Fake)
	fake.RunWithinFn = func(_ time.Duration, name string, args ...string) (string, bool, error) {
		if name != "op" || len(args) != 2 || args[0] != "read" {
			t.Fatalf("unexpected command %s %v", name, args)
		}
		return "fixture-resolved-value", false, nil
	}
	var out bytes.Buffer
	seams := setupSeamsFor(t, dir, &setupFakeDocker{}, &setupFakeMCP{})
	seams.Env = env
	input := "op://fixture/anthropic/key\nop://fixture/openai/key\nop://fixture/google/key\nop://fixture/parallel/key\nop://fixture/github/key\n1\n"
	err := (&setupCmd{}).run(&cli.Deps{Out: &out, Err: &out, In: strings.NewReader(input), Interactive: true}, seams)
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "PARALLEL_API_KEY", "GITHUB_TOKEN"} {
		if !strings.Contains(readFile(t, filepath.Join(home, "secrets.env")), key+"=op://fixture/") {
			t.Errorf("missing %s", key)
		}
	}
	for _, label := range []string{"Anthropic:", "OpenAI:", "Google:", "Parallel web search:", "GitHub:"} {
		if at := strings.Index(out.String(), label); at < 0 || at > strings.Index(out.String(), "Choose your model") {
			t.Errorf("connection not offered before model: %s", label)
		}
	}
	for _, label := range []string{"GPT-6 Astra", "Claude Fable 5.1", "Gemini 3.8 Flash"} {
		if !strings.Contains(out.String(), label) {
			t.Errorf("picker omitted %s", label)
		}
	}
	if strings.Contains(out.String(), "fixture-resolved-value") || strings.Contains(readFile(t, filepath.Join(home, "secrets.env")), "fixture-resolved-value") {
		t.Fatal("credential leaked")
	}
	before := readFile(t, filepath.Join(home, "envs/default/pix.toml"))
	out.Reset()
	if err := (&setupCmd{}).run(&cli.Deps{Out: &out, Err: &out, In: strings.NewReader(""), Interactive: true}, seams); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "GitHub:") || strings.Contains(out.String(), "Parallel web search:") || strings.Contains(out.String(), "Model number:") || before != readFile(t, filepath.Join(home, "envs/default/pix.toml")) {
		t.Fatal("rerun repeated completed setup")
	}
}

func TestSetupConnectionsSkipNeedsNoOnePassword(t *testing.T) {
	home, _ := modelSetupHome(t, "")
	env := hostenv.Env{System: &systest.Fake{Base: sys.Real{}, LookPathFn: func(string) (string, error) { t.Fatal("skipping connections must not look for op"); return "", nil }}}
	var out bytes.Buffer
	if err := setupOptionalConnections(&cli.Deps{Out: &out, Err: &out, In: strings.NewReader("\n\n\n\n\n"), Interactive: true}, home, env, "default"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home.Home, "secrets.env")); !os.IsNotExist(err) {
		t.Fatal("skip wrote credentials")
	}
}

func TestSetupConnectionsEnvironmentBackendSkipsPersonalInterview(t *testing.T) {
	home, path := modelSetupHome(t, "")
	if err := os.WriteFile(path, []byte(gatewaySidecar), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { t.Fatal("gateway must not read personal references"); return "", nil }}}
	if err := setupOptionalConnections(&cli.Deps{Out: &out, Err: &out, In: strings.NewReader(""), Interactive: true}, home, env, "default"); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("gateway interview: %s", out.String())
	}
}

func TestSetupConnectionsResumeMissingOnlyAndRejectUnreadableReference(t *testing.T) {
	home, _ := modelSetupHome(t, "OPENAI_API_KEY=op://fixture/existing/key\n")
	var out bytes.Buffer
	env := hostenv.Env{System: &systest.Fake{Base: sys.Real{}, LookPathFn: func(string) (string, error) { return "/stub/op", nil }, RunWithinFn: func(_ time.Duration, _ string, _ ...string) (string, bool, error) {
		return "fixture-must-not-leak", true, nil
	}}}
	d := &cli.Deps{Out: &out, Err: &out, In: strings.NewReader("op://fixture/unreadable/key\n\n\n\n\n"), Interactive: true}
	if err := setupOptionalConnections(d, home, env, "default"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "OpenAI:") || strings.Contains(out.String(), "fixture-must-not-leak") {
		t.Fatalf("bad output: %s", out.String())
	}
	if got := readFile(t, filepath.Join(home.Home, "secrets.env")); got != "OPENAI_API_KEY=op://fixture/existing/key\n" {
		t.Fatalf("failed probe changed refs: %s", got)
	}
}

func TestSetupConnectionsAddsOnlyMissingGitHub(t *testing.T) {
	refs := "ANTHROPIC_API_KEY=op://fixture/anthropic/key\nOPENAI_API_KEY=op://fixture/openai/key\nGEMINI_API_KEY=op://fixture/google/key\nPARALLEL_API_KEY=op://fixture/parallel/key\n"
	home, sidecar := modelSetupHome(t, refs)
	before := readFile(t, sidecar)
	var out bytes.Buffer
	env := hostenv.Env{System: &systest.Fake{Base: sys.Real{}, LookPathFn: func(string) (string, error) { return "/stub/op", nil }, RunWithinFn: func(_ time.Duration, name string, args ...string) (string, bool, error) {
		if name != "op" || len(args) != 2 || args[0] != "read" || args[1] != "op://pix/GITHUB_TOKEN/credential" {
			t.Fatalf("unexpected reference check: %s %v", name, args)
		}
		return "fixture-github-value", false, nil
	}}}
	d := &cli.Deps{Out: &out, Err: &out, In: strings.NewReader("\"op://pix/GITHUB_TOKEN/credential\"\n"), Interactive: true}
	if err := setupOptionalConnections(d, home, env, "default"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "GitHub:") {
		t.Fatal("missing GitHub prompt")
	}
	for _, label := range []string{"Anthropic:", "OpenAI:", "Google:", "Parallel web search:"} {
		if strings.Contains(out.String(), label) {
			t.Fatalf("repeated %s", label)
		}
	}
	saved := readFile(t, home.SecretsEnv)
	if saved != refs+"GITHUB_TOKEN=op://pix/GITHUB_TOKEN/credential\n" {
		t.Fatalf("unexpected saved references: %s", saved)
	}
	if strings.Contains(out.String(), "fixture-github-value") || before != readFile(t, sidecar) {
		t.Fatal("disclosed credential or changed model settings")
	}
	info, err := os.Stat(home.SecretsEnv)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("references lost private file permissions", err)
	}
}
