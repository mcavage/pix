// setup_modelreceipt_test.go: AC-06/07 (Unit U2) — setup always names
// watcher/embed/bridge Ollama model readiness before handoff, using the SAME
// shared modelreadiness.go vocabulary doctor uses, and never downloads by
// default. See setup.go's setupModelReceipt + extractPullModelsFlag.
package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"pi-stack/host/config"
)

func modelReceiptCfg() *config.Config {
	c := &config.Config{}
	c.MemoryWatcherModel = "qwen3.5:9b"
	c.MemoryEmbedModel = "nomic-embed-text"
	c.OllamaBridgeModel = "qwen3.5:9b"
	return c
}

// --- extractPullModelsFlag: a SETUP-only flag, never routed through onboard's parser. ---

func TestExtractPullModelsFlag_PresentIsStripped(t *testing.T) {
	pull, rest := extractPullModelsFlag([]string{"--yes", "--pull-models", "--mcp", "gog"})
	if !pull {
		t.Error("expected pull=true")
	}
	want := []string{"--yes", "--mcp", "gog"}
	if len(rest) != len(want) {
		t.Fatalf("rest = %v, want %v", rest, want)
	}
	for i := range want {
		if rest[i] != want[i] {
			t.Fatalf("rest = %v, want %v", rest, want)
		}
	}
}

func TestExtractPullModelsFlag_Absent(t *testing.T) {
	pull, rest := extractPullModelsFlag([]string{"--yes"})
	if pull {
		t.Error("expected pull=false")
	}
	if len(rest) != 1 || rest[0] != "--yes" {
		t.Errorf("rest = %v, want [--yes]", rest)
	}
}

// --- setupModelReceipt: always names watcher/embed/bridge (AC-06/07). ---

func TestSetupModelReceipt_AllHealthy_NamesAllThreeNoPrompt(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(int) bool { return true },
		run: func(name string, args ...string) (string, error) {
			if name == "ollama" && len(args) == 1 && args[0] == "list" {
				return "NAME\nqwen3.5:9b:latest\nnomic-embed-text:latest\n", nil
			}
			t.Fatalf("unexpected run(%s, %v) — no pull should happen when everything is healthy", name, args)
			return "", nil
		},
	}
	var out bytes.Buffer
	// Empty reader: if this blocked on a prompt read, the test would hang/fail.
	setupModelReceipt(env, &out, strings.NewReader(""), modelReceiptCfg(), true, false)
	got := out.String()
	for _, want := range []string{"watcher", "embed", "bridge"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected receipt to name %q, got:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not pulled") || strings.Contains(got, "Pull missing") {
		t.Errorf("all-healthy receipt must not mention missing/pull, got:\n%s", got)
	}
}

func TestSetupModelReceipt_OllamaNotInstalled_NoPromptNoCrash(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
	}
	var out bytes.Buffer
	setupModelReceipt(env, &out, strings.NewReader(""), modelReceiptCfg(), true, false)
	got := out.String()
	if !strings.Contains(got, "install ollama") {
		t.Errorf("expected an install-ollama note, got:\n%s", got)
	}
	if strings.Contains(got, "Pull missing") {
		t.Error("must not offer to pull when ollama isn't even installed")
	}
}

// TestSetupModelReceipt_NonInteractive_DeferredCommandsOnly: no TTY / --yes,
// no --pull-models -> NEVER downloads; prints the exact deferred commands.
func TestSetupModelReceipt_NonInteractive_DeferredCommandsOnly(t *testing.T) {
	pullCalls := 0
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(int) bool { return true },
		run: func(name string, args ...string) (string, error) {
			if len(args) >= 1 && args[0] == "list" {
				return "NAME\n", nil // nothing pulled
			}
			pullCalls++
			return "", nil
		},
	}
	var out bytes.Buffer
	// interactive=false, forcePull=false — must never read from `in` either.
	setupModelReceipt(env, &out, strings.NewReader(""), modelReceiptCfg(), false, false)
	if pullCalls != 0 {
		t.Errorf("non-interactive setup must never pull automatically, got %d pull calls", pullCalls)
	}
	got := out.String()
	if !strings.Contains(got, "ollama pull qwen3.5:9b") {
		t.Errorf("expected the exact deferred pull command for qwen3.5:9b, got:\n%s", got)
	}
	if !strings.Contains(got, "ollama pull nomic-embed-text") {
		t.Errorf("expected the exact deferred pull command for nomic-embed-text, got:\n%s", got)
	}
	// Dedup: qwen3.5:9b backs BOTH watcher and bridge — only one pull command.
	if n := strings.Count(got, "ollama pull qwen3.5:9b"); n != 1 {
		t.Errorf("expected the shared qwen3.5:9b tag deduped to one deferred command, got %d occurrences:\n%s", n, got)
	}
}

// TestSetupModelReceipt_Interactive_DefaultNoDeclines: a bare Enter on the
// prompt must decline (default No) and fall back to deferred commands, never
// pull.
func TestSetupModelReceipt_Interactive_DefaultNoDeclines(t *testing.T) {
	pullCalls := 0
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(int) bool { return true },
		run: func(name string, args ...string) (string, error) {
			if len(args) >= 1 && args[0] == "list" {
				return "NAME\n", nil
			}
			pullCalls++
			return "", nil
		},
	}
	var out bytes.Buffer
	setupModelReceipt(env, &out, strings.NewReader("\n"), modelReceiptCfg(), true, false)
	if pullCalls != 0 {
		t.Errorf("declining the prompt must never pull, got %d pull calls", pullCalls)
	}
	if !strings.Contains(out.String(), "ollama pull qwen3.5:9b") {
		t.Errorf("expected deferred commands after declining, got:\n%s", out.String())
	}
}

// TestSetupModelReceipt_Interactive_YesPullsDeduped: answering yes pulls each
// DISTINCT missing tag exactly once, even though qwen3.5:9b backs two roles.
func TestSetupModelReceipt_Interactive_YesPullsDeduped(t *testing.T) {
	pulled := map[string]int{}
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(int) bool { return true },
		run: func(name string, args ...string) (string, error) {
			if len(args) >= 1 && args[0] == "list" {
				return "NAME\n", nil
			}
			if len(args) == 2 && args[0] == "pull" {
				pulled[args[1]]++
				return "success", nil
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	setupModelReceipt(env, &out, strings.NewReader("y\n"), modelReceiptCfg(), true, false)
	if pulled["qwen3.5:9b"] != 1 {
		t.Errorf("expected qwen3.5:9b pulled exactly once (dedup), got %d", pulled["qwen3.5:9b"])
	}
	if pulled["nomic-embed-text"] != 1 {
		t.Errorf("expected nomic-embed-text pulled exactly once, got %d", pulled["nomic-embed-text"])
	}
	if !strings.Contains(out.String(), "pulled") {
		t.Errorf("expected a pulled receipt line, got:\n%s", out.String())
	}
}

// TestSetupModelReceipt_ForcePull_NoPromptEvenWithEmptyReader: --pull-models
// forces pulls without ever reading a prompt (an empty reader must not hang).
func TestSetupModelReceipt_ForcePull_NoPromptEvenWithEmptyReader(t *testing.T) {
	pulled := map[string]int{}
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(int) bool { return true },
		run: func(name string, args ...string) (string, error) {
			if len(args) >= 1 && args[0] == "list" {
				return "NAME\n", nil
			}
			if len(args) == 2 && args[0] == "pull" {
				pulled[args[1]]++
				return "success", nil
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	// interactive=false (non-TTY / --yes) AND forcePull=true: must still pull.
	setupModelReceipt(env, &out, strings.NewReader(""), modelReceiptCfg(), false, true)
	if pulled["qwen3.5:9b"] != 1 || pulled["nomic-embed-text"] != 1 {
		t.Errorf("expected --pull-models to force both tags pulled once each, got %v", pulled)
	}
}

// TestSetupModelReceipt_PullFailure_ReceiptedNotFatal: a pull failure must be
// receipted clearly and setup must continue — never claim success, never
// panic/abort.
func TestSetupModelReceipt_PullFailure_ReceiptedNotFatal(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(int) bool { return true },
		run: func(name string, args ...string) (string, error) {
			if len(args) >= 1 && args[0] == "list" {
				return "NAME\n", nil
			}
			if len(args) == 2 && args[0] == "pull" && args[1] == "qwen3.5:9b" {
				return "", fmt.Errorf("not found")
			}
			return "success", nil
		},
	}
	var out bytes.Buffer
	setupModelReceipt(env, &out, strings.NewReader(""), modelReceiptCfg(), false, true)
	got := out.String()
	if !strings.Contains(got, "qwen3.5:9b") || !strings.Contains(got, "failed") {
		t.Errorf("expected a clear pull-failure receipt for qwen3.5:9b, got:\n%s", got)
	}
	if !strings.Contains(got, "nomic-embed-text") {
		t.Errorf("expected setup to continue and still receipt the other model, got:\n%s", got)
	}
}
