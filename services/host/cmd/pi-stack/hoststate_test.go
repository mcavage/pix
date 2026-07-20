package main

import (
	"strings"
	"testing"

	"pi-stack/host/config"
)

func TestBuildHostState(t *testing.T) {
	cfg := &config.Config{
		GogAccount:         "me@acme.com",
		MCP:                []string{"gog"},
		KnowledgeBundles:   []string{"/kb/acme"},
		MemoryWatcherModel: "gemma4:e4b-mlx",
		MemoryEmbedModel:   "nomic-embed-text",
	}
	cfg.Kits.Stack = []string{"/repos/pi-stack-work/kit"}
	// sbx secret ls output that marks anthropic present (secretCheck parses this).
	sbxOut := "anthropic\ngithub\n"
	up := func(int) bool { return true }

	hs := buildHostState(cfg, sbxOut, true, up, true, "1password", hostStatePack{Active: true, Path: "/kb/acme", GitInitialized: true, Skills: true})
	if !hs.Pack.Active || !hs.Pack.GitInitialized {
		t.Errorf("pack facts not carried: %+v", hs.Pack)
	}
	if hs.Keys.Source != "1password" {
		t.Errorf("keys source = %q, want 1password", hs.Keys.Source)
	}

	if !hs.Keys.Anthropic || !hs.Keys.Resolved {
		t.Errorf("anthropic key should resolve: %+v", hs.Keys)
	}
	if hs.Keys.OpenAI || hs.Keys.Google {
		t.Errorf("openai/google should be absent: %+v", hs.Keys)
	}
	if !hs.Memory.Up || hs.Memory.Port != memoryPortDefault {
		t.Errorf("memory up/port wrong: %+v", hs.Memory)
	}
	if !hs.Knowledge.Seeded || len(hs.Knowledge.Bundles) != 1 {
		t.Errorf("knowledge should be seeded: %+v", hs.Knowledge)
	}
	if !hs.Gog.Enabled || hs.Gog.Account != "me@acme.com" {
		t.Errorf("gog wrong: %+v", hs.Gog)
	}
	if !hs.MCP.Enabled || len(hs.MCP.Servers) != 1 {
		t.Errorf("mcp wrong: %+v", hs.MCP)
	}
	if hs.Overlay.Kit != "kit" {
		t.Errorf("overlay kit basename wrong: %q", hs.Overlay.Kit)
	}
	if !hs.Provisioned {
		t.Error("keys+knowledge+overlay present => provisioned")
	}
	if hs.Models.Watcher != "gemma4:e4b-mlx" {
		t.Errorf("watcher model wrong: %q", hs.Models.Watcher)
	}
}

func TestBuildHostState_NotProvisioned(t *testing.T) {
	cfg := &config.Config{MemoryWatcherModel: "x", MemoryEmbedModel: "y"}
	hs := buildHostState(cfg, "", false, func(int) bool { return false }, false, "", hostStatePack{})
	if hs.Keys.Source != "sbx" {
		t.Errorf("default keys source = %q, want sbx", hs.Keys.Source)
	}
	if hs.Provisioned {
		t.Error("empty host must not be provisioned")
	}
	if hs.Keys.Resolved {
		t.Error("no secrets => keys not resolved")
	}
	if hs.MCP.Enabled {
		t.Error("gateway off => mcp disabled")
	}
	// JSON must never leak a secret value: it only has booleans/names.
	if strings.Contains(hs.Keys.Source, "sk-") {
		t.Error("source must not contain a key value")
	}
}

func TestReadGitIdentity(t *testing.T) {
	env := shellEnv{
		run: func(name string, args ...string) (string, error) {
			if name == "git" && len(args) == 3 && args[2] == "user.name" {
				return "Mark C\n", nil
			}
			if name == "git" && len(args) == 3 && args[2] == "user.email" {
				return "mark@example.com\n", nil
			}
			return "", nil
		},
	}
	id := readGitIdentity(env)
	if id.Name != "Mark C" || id.Email != "mark@example.com" {
		t.Errorf("git identity not read: %+v", id)
	}
	// No git / nil run -> empty, no panic.
	if got := readGitIdentity(shellEnv{}); got.Name != "" || got.Email != "" {
		t.Errorf("expected empty identity with no run, got %+v", got)
	}
}
