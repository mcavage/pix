package main

import (
	"strings"
	"testing"

	"pi-stack/host/config"
)

// contains reports whether the ordered args slice contains the given
// consecutive subsequence.
func contains(args, sub []string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(args); i++ {
		if match(args[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

func match(a, b []string) bool {
	for i := range b {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// countFlag counts how many times a --flag appears in args.
func countFlag(args []string, flag string) int {
	n := 0
	for _, a := range args {
		if a == flag {
			n++
		}
	}
	return n
}

func TestKitRef(t *testing.T) {
	if got := kitRef("0.0.99"); got != "v0.0.99" {
		t.Errorf("kitRef(0.0.99) = %q, want v0.0.99", got)
	}
	if got := kitRef("dev"); got != "main" {
		t.Errorf("kitRef(dev) = %q, want main", got)
	}
}

func TestBuildSbxArgs_VersionPin(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: "."}, "0.0.99")

	if args[0] != "run" || args[1] != "pi-stack" {
		t.Fatalf("args should start with `run pi-stack`, got %v", args[:2])
	}
	want := "git+https://github.com/mcavage/pi-stack.git#ref=v0.0.99&dir=pi-kit"
	if !contains(args, []string{"--kit", want}) {
		t.Errorf("expected pinned kit %q in %v", want, args)
	}
}

func TestBuildSbxArgs_DevVersionTracksMain(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: "."}, "dev")

	want := "git+https://github.com/mcavage/pi-stack.git#ref=main&dir=pi-kit"
	if !contains(args, []string{"--kit", want}) {
		t.Errorf("dev build should track #ref=main, got %v", args)
	}
	// Must NOT contain a v-prefixed tag.
	for _, a := range args {
		if strings.Contains(a, "#ref=vdev") {
			t.Errorf("dev build produced a v-tag pin: %q", a)
		}
	}
}

func TestBuildSbxArgs_KitStacking(t *testing.T) {
	cfg := &config.Config{}
	cfg.Kits.Stack = []string{"/overlay/kit", "git+https://example.com/other#dir=kit"}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", Kits: []string{"/flag/kit"}}, "0.0.99")

	// git kit + 2 config stack + 1 flag kit = 4 --kit total.
	if got := countFlag(args, "--kit"); got != 4 {
		t.Errorf("expected 4 --kit flags, got %d in %v", got, args)
	}
	if !contains(args, []string{"--kit", "/overlay/kit"}) {
		t.Errorf("config stack kit missing from %v", args)
	}
	if !contains(args, []string{"--kit", "/flag/kit"}) {
		t.Errorf("flag kit missing from %v", args)
	}
}

func TestBuildSbxArgs_MCPExpansion(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack", "notion"}}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", MCP: []string{"linear"}}, "0.0.99")

	if got := countFlag(args, "--mcp"); got != 3 {
		t.Errorf("expected 3 --mcp flags, got %d in %v", got, args)
	}
	for _, m := range []string{"slack", "notion", "linear"} {
		if !contains(args, []string{"--mcp", m}) {
			t.Errorf("--mcp %s missing from %v", m, args)
		}
	}
}

// TestBuildSbxArgs_TokenNeverOnArgv is the F1 SECURITY guard: the broker bearer
// must NEVER appear on the composed sbx argv (argv is process-inspectable via
// ps/EDR). run.go injects it into the sbx CHILD PROCESS ENV instead; the arg
// builder must stay token-free even when a token is present.
func TestBuildSbxArgs_TokenNeverOnArgv(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", Token: "abc123"}, "0.0.99")
	for _, a := range args {
		if strings.Contains(a, "abc123") || strings.Contains(a, "GWS_TOKEN_AUTH") {
			t.Errorf("broker token leaked onto argv %v (must go in the process env only)", args)
		}
	}
	if countFlag(args, "--env") != 0 {
		t.Errorf("arg builder must emit no --env flags, got %v", args)
	}
}

func TestBuildSbxArgs_DevBranch(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", Dev: true, DevRoot: "/repo"}, "0.0.99")

	// Dev mode uses the local kit, NOT the git kit.
	if !contains(args, []string{"--kit", "/repo/pi-kit"}) {
		t.Errorf("dev mode should use local kit /repo/pi-kit, got %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "git+") {
			t.Errorf("dev mode must not use a git kit, found %q", a)
		}
	}
	// Mode B: skills loaded live via passthrough.
	if !contains(args, []string{"--no-skills", "--skill", "/repo/skills"}) {
		t.Errorf("dev mode should pass --no-skills --skill /repo/skills, got %v", args)
	}
	// Repo skills mounted as an extra workspace.
	if !contains(args, []string{"/repo/skills"}) {
		t.Errorf("dev mode should mount /repo/skills workspace, got %v", args)
	}
}

func TestBuildSbxArgs_NameModelPassthrough(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{
		Workspace:   "/work",
		Name:        "t",
		Model:       "openai/gpt-5.6-sol",
		Passthrough: []string{"--resume"},
	}, "0.0.99")

	if !contains(args, []string{"--name", "t"}) {
		t.Errorf("--name missing from %v", args)
	}
	if !contains(args, []string{"--model", "openai/gpt-5.6-sol"}) {
		t.Errorf("--model should be passed through to pi, got %v", args)
	}
	if !contains(args, []string{"--", "--model", "openai/gpt-5.6-sol", "--resume"}) {
		t.Errorf("pi args should follow -- in order, got %v", args)
	}
	if !contains(args, []string{"--name", "t", "--kit"}) {
		t.Errorf("--name should precede the kit, got %v", args)
	}
}

func TestBuildSbxArgs_LiveSkillsMountedAndLoaded(t *testing.T) {
	cfg := &config.Config{}
	cfg.Skills.Paths = []string{"/cfg/skills"}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", Skills: []string{"/flag/skills"}}, "0.0.99")

	for _, s := range []string{"/cfg/skills", "/flag/skills"} {
		// Mounted as workspace (bare arg) and loaded via --skill.
		if !contains(args, []string{s}) {
			t.Errorf("skill dir %s not mounted in %v", s, args)
		}
		if !contains(args, []string{"--skill", s}) {
			t.Errorf("skill dir %s not loaded via --skill in %v", s, args)
		}
	}
}

func TestParseRunArgs(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want runOpts
	}{
		{"defaults", nil, runOpts{Workspace: "."}},
		{"dir first", []string{"/tmp"}, runOpts{Workspace: "/tmp"}},
		{"flags then dir", []string{"--name", "t", "/tmp"}, runOpts{Workspace: "/tmp", Name: "t"}},
		{"dir then flags", []string{"/tmp", "--name", "t"}, runOpts{Workspace: "/tmp", Name: "t"}},
		{"eq form", []string{"--name=t", "--model=m"}, runOpts{Workspace: ".", Name: "t", Model: "m"}},
		{"dev", []string{"--dev"}, runOpts{Workspace: ".", Dev: true}},
		{"repeatable", []string{"--mcp", "a", "--mcp", "b", "--kit", "k"}, runOpts{Workspace: ".", MCP: []string{"a", "b"}, Kits: []string{"k"}}},
		{"passthrough", []string{"/tmp", "--", "--model", "x"}, runOpts{Workspace: "/tmp", Passthrough: []string{"--model", "x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRunArgs(tt.argv)
			if err != nil {
				t.Fatalf("parseRunArgs(%v) error: %v", tt.argv, err)
			}
			if got.Workspace != tt.want.Workspace || got.Name != tt.want.Name ||
				got.Model != tt.want.Model || got.Dev != tt.want.Dev {
				t.Errorf("parseRunArgs(%v) = %+v, want %+v", tt.argv, got, tt.want)
			}
			if !equalSlice(got.MCP, tt.want.MCP) || !equalSlice(got.Kits, tt.want.Kits) ||
				!equalSlice(got.Passthrough, tt.want.Passthrough) {
				t.Errorf("parseRunArgs(%v) slices = %+v, want %+v", tt.argv, got, tt.want)
			}
		})
	}
}

func TestParseRunArgs_Errors(t *testing.T) {
	if _, err := parseRunArgs([]string{"--bogus"}); err == nil {
		t.Error("expected error for unknown flag")
	}
	if _, err := parseRunArgs([]string{"/a", "/b"}); err == nil {
		t.Error("expected error for two positional dirs")
	}
	if _, err := parseRunArgs([]string{"--name"}); err == nil {
		t.Error("expected error for flag missing value")
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
