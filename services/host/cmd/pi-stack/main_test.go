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
	// An unreleased +local build must NOT pin a v-tag.
	if got := kitRef("0.0.16+local"); got != "main" {
		t.Errorf("kitRef(0.0.16+local) = %q, want main", got)
	}
}

func TestIsReleased(t *testing.T) {
	released := []string{"0.0.16", "1.2.3", "10.20.30"}
	unreleased := []string{"dev", "0.0.16+local", "0.0.16-dev", "v0.0.16", "0.0", ""}
	for _, v := range released {
		if !isReleased(v) {
			t.Errorf("isReleased(%q) = false, want true", v)
		}
	}
	for _, v := range unreleased {
		if isReleased(v) {
			t.Errorf("isReleased(%q) = true, want false", v)
		}
	}
}

func TestBuildSbxArgs_UnreleasedWithCheckout(t *testing.T) {
	cfg := &config.Config{}
	// Caller resolved a local checkout + image tag for an unreleased build.
	args := buildSbxArgs(cfg, runOpts{
		Workspace:     ".",
		LocalKit:      "/repo/pi-kit",
		LocalImageTag: "local-123",
	}, "0.0.16+local")

	if !contains(args, []string{"--kit", "/repo/pi-kit"}) {
		t.Errorf("unreleased+checkout should use local kit, got %v", args)
	}
	if !contains(args, []string{"--template", "docker.io/mcavage/pi-stack:local-123"}) {
		t.Errorf("unreleased+checkout should pin --template from .local-image-tag, got %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "git+") {
			t.Errorf("unreleased+checkout must not pin a git kit, found %q", a)
		}
	}
}

func TestBuildSbxArgs_UnreleasedWithCheckoutNoImageTag(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", LocalKit: "/repo/pi-kit"}, "0.0.16+local")
	if !contains(args, []string{"--kit", "/repo/pi-kit"}) {
		t.Errorf("should use local kit, got %v", args)
	}
	if countFlag(args, "--template") != 0 {
		t.Errorf("no .local-image-tag => no --template, got %v", args)
	}
}

func TestBuildSbxArgs_UnreleasedNoCheckoutTracksMain(t *testing.T) {
	cfg := &config.Config{}
	// No LocalKit resolved (no checkout): fall back to #ref=main, never v<ver>.
	args := buildSbxArgs(cfg, runOpts{Workspace: "."}, "0.0.16+local")
	want := "git+https://github.com/mcavage/pi-stack.git#ref=main&dir=pi-kit"
	if !contains(args, []string{"--kit", want}) {
		t.Errorf("unreleased+no-checkout should track #ref=main, got %v", args)
	}
	for _, a := range args {
		if strings.Contains(a, "#ref=v") {
			t.Errorf("unreleased build produced a v-tag pin: %q", a)
		}
	}
}

func TestBuildSbxArgs_KitOverrideWins(t *testing.T) {
	cfg := &config.Config{}
	// --kit is an escape hatch even for an unreleased build with a checkout.
	args := buildSbxArgs(cfg, runOpts{
		Workspace:     ".",
		Kits:          []string{"/my/kit"},
		LocalKit:      "/repo/pi-kit",
		LocalImageTag: "local-123",
	}, "0.0.16+local")

	if !contains(args, []string{"--kit", "/my/kit"}) {
		t.Errorf("--kit override missing, got %v", args)
	}
	if countFlag(args, "--kit") != 1 {
		t.Errorf("--kit override should be the only kit, got %v", args)
	}
	if countFlag(args, "--template") != 0 {
		t.Errorf("--kit override should suppress the auto --template, got %v", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "git+") {
			t.Errorf("--kit override must suppress the git pin, found %q", a)
		}
	}
}

func TestKitResolveFailureMsg(t *testing.T) {
	// No git-ref kit -> no message (unrelated failure).
	if msg := kitResolveFailureMsg(""); msg != "" {
		t.Errorf("expected empty message for no pinned kit, got %q", msg)
	}
	if msg := kitResolveFailureMsg(pinnedGitKit([]string{"--kit", "/repo/pi-kit"})); msg != "" {
		t.Errorf("local kit should not trigger the message, got %q", msg)
	}
	// A v-tag pin mentions the release.
	vtag := "git+https://github.com/mcavage/pi-stack.git#ref=v0.0.16&dir=pi-kit"
	msg := kitResolveFailureMsg(pinnedGitKit([]string{"--kit", vtag}))
	if !strings.Contains(msg, "v0.0.16") || !strings.Contains(msg, "--dev") || !strings.Contains(msg, "--kit") {
		t.Errorf("v-tag failure message missing key hints: %q", msg)
	}
	// A main pin still explains.
	mainRef := "git+https://github.com/mcavage/pi-stack.git#ref=main&dir=pi-kit"
	if msg := kitResolveFailureMsg(pinnedGitKit([]string{"--kit", mainRef})); !strings.Contains(msg, "main") {
		t.Errorf("main failure message should mention the ref, got %q", msg)
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

	// --kit override (1 flag kit, base) + 2 config stack = 3 --kit total. The
	// override suppresses the auto git pin.
	if got := countFlag(args, "--kit"); got != 3 {
		t.Errorf("expected 3 --kit flags, got %d in %v", got, args)
	}
	if !contains(args, []string{"--kit", "/overlay/kit"}) {
		t.Errorf("config stack kit missing from %v", args)
	}
	if !contains(args, []string{"--kit", "/flag/kit"}) {
		t.Errorf("flag kit missing from %v", args)
	}
	// The escape-hatch override suppresses the launcher's git pin.
	if pinnedGitKit(args) != "" {
		t.Errorf("--kit override should suppress the git pin, got %v", args)
	}
}

func TestBuildSbxArgs_StackWithoutOverride(t *testing.T) {
	cfg := &config.Config{}
	cfg.Kits.Stack = []string{"/overlay/kit"}
	args := buildSbxArgs(cfg, runOpts{Workspace: "."}, "0.0.99")
	// Released base git kit + 1 config stack = 2.
	if got := countFlag(args, "--kit"); got != 2 {
		t.Errorf("expected 2 --kit flags, got %d in %v", got, args)
	}
	if pinnedGitKit(args) == "" {
		t.Errorf("released build with no override should pin the git kit, got %v", args)
	}
}

func TestBuildSbxArgs_MCPExpansion(t *testing.T) {
	cfg := &config.Config{}
	// buildSbxArgs emits --static-mcp for the RESOLVED static set (o.StaticMCP);
	// the caller computes it via resolveStaticMCP. The sbx local gateway serves
	// them, no SBX_MCP_URL.
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", StaticMCP: []string{"slack", "notion", "linear"}}, "0.0.99")

	if got := countFlag(args, "--static-mcp"); got != 3 {
		t.Errorf("expected 3 --static-mcp flags, got %d in %v", got, args)
	}
	if got := countFlag(args, "--mcp"); got != 0 {
		t.Errorf("must emit --static-mcp, never the removed --mcp; got %d in %v", got, args)
	}
	for _, m := range []string{"slack", "notion", "linear"} {
		if !contains(args, []string{"--static-mcp", m}) {
			t.Errorf("--static-mcp %s missing from %v", m, args)
		}
	}
}

// resolveStaticMCP: default dynamic for every server; only mcp_static pins eager;
// mcp_dynamic wins if a server is in both.
func TestResolveStaticMCP(t *testing.T) {
	// Default: nothing configured -> nothing static (all dynamic), local or remote.
	if got := resolveStaticMCP([]string{"slack", "notion", "gog"}, &config.Config{}); len(got) != 0 {
		t.Errorf("default must be all-dynamic (empty static set), got %v", got)
	}
	// mcp_static pins eager; mcp_dynamic wins over mcp_static; order preserved.
	cfg := &config.Config{
		MCPStatic:  []string{"slack", "notion"},
		MCPDynamic: []string{"notion"}, // overrides its own static entry
	}
	got := resolveStaticMCP([]string{"gog", "slack", "notion", "atlassian"}, cfg)
	// slack: static. notion: static-but-dynamic-override -> dropped. gog/atlassian: default dynamic.
	if len(got) != 1 || got[0] != "slack" {
		t.Errorf("resolveStaticMCP = %v, want [slack]", got)
	}
}

// resolveStaticMCPForRun is what run.go/task.go actually call: config-listed
// servers (cfg.MCP) keep the default-dynamic/mcp_static semantics of
// resolveStaticMCP, but an EXPLICIT per-run `--mcp` flag is a one-run promise
// to attach eagerly, so it must be emitted as --static-mcp on create unless
// the user pins it dynamic via the stronger, documented mcp_dynamic override.
func TestResolveStaticMCPForRun(t *testing.T) {
	// Explicit --mcp with no config at all: still eager (this is finding #1 --
	// the bug was these got silently filtered to dynamic).
	got := resolveStaticMCPForRun(nil, []string{"linear"}, &config.Config{})
	if len(got) != 1 || got[0] != "linear" {
		t.Fatalf("explicit --mcp must be eager by default, got %v", got)
	}

	// mcp_dynamic is the documented stronger override: it still wins even over
	// an explicit --mcp for the same server.
	cfgDynamic := &config.Config{MCPDynamic: []string{"linear"}}
	got = resolveStaticMCPForRun(nil, []string{"linear"}, cfgDynamic)
	if len(got) != 0 {
		t.Errorf("mcp_dynamic must override an explicit --mcp, got %v", got)
	}

	// Config-listed servers (cfg.MCP, not explicit --mcp) keep the plain
	// default-dynamic behavior: unlisted in mcp_static -> stays dynamic.
	got = resolveStaticMCPForRun([]string{"notion"}, nil, &config.Config{})
	if len(got) != 0 {
		t.Errorf("cfg.MCP alone must stay dynamic by default, got %v", got)
	}

	// A server that is BOTH config-listed and mcp_static stays eager (unchanged
	// resolveStaticMCP behavior), alongside an explicit --mcp for another server,
	// preserving order and de-duplication.
	cfgMixed := &config.Config{MCPStatic: []string{"notion"}}
	got = resolveStaticMCPForRun([]string{"notion", "gog"}, []string{"linear", "gog"}, cfgMixed)
	if !equalSlice(got, []string{"notion", "gog", "linear"}) {
		t.Errorf("resolveStaticMCPForRun = %v, want [notion gog linear]", got)
	}
}

// TestBuildSbxArgs_ExplicitMCPFlagIsStatic is the parse-to-argv integration
// test the review asked for: an explicit `pi-stack run --mcp M` on a DEFAULT
// config must reach sbx as --static-mcp, not be silently dropped to dynamic.
func TestBuildSbxArgs_ExplicitMCPFlagIsStatic(t *testing.T) {
	cfg := &config.Config{} // default config: no mcp_static/mcp_dynamic at all
	o, err := parseRunArgs([]string{"--mcp", "linear"})
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	o.StaticMCP = resolveStaticMCPForRun(cfg.MCP, o.MCP, cfg)
	args := buildSbxArgs(cfg, o, "0.0.99")
	if !contains(args, []string{"--static-mcp", "linear"}) {
		t.Errorf("explicit --mcp linear must be emitted as --static-mcp on default config, got %v", args)
	}

	// Same explicit flag, but the user pinned it mcp_dynamic (the stronger,
	// documented override) -- now it must NOT be static.
	cfgDynamic := &config.Config{MCPDynamic: []string{"linear"}}
	o2, err := parseRunArgs([]string{"--mcp", "linear"})
	if err != nil {
		t.Fatalf("parseRunArgs: %v", err)
	}
	o2.StaticMCP = resolveStaticMCPForRun(cfgDynamic.MCP, o2.MCP, cfgDynamic)
	args2 := buildSbxArgs(cfgDynamic, o2, "0.0.99")
	if contains(args2, []string{"--static-mcp", "linear"}) {
		t.Errorf("mcp_dynamic override must keep explicit --mcp lazy, got %v", args2)
	}
}

// A pack integration with static=true folds into cfg.MCPStatic (eager); a user
// mcp_dynamic still overrides it back to lazy.
func TestPackStaticMcpNames_AndOverride(t *testing.T) {
	p := &packInfo{Manifest: packManifest{Integrations: []packIntegration{
		{Name: "Fastmail", MCP: "fastmail", Static: true}, // eager
		{Name: "Notion", MCP: "notion"},                   // default dynamic
		{Name: "NoServer"},                                // no mcp -> ignored
	}}}
	if got := packStaticMcpNames(p); len(got) != 1 || got[0] != "fastmail" {
		t.Fatalf("packStaticMcpNames = %v, want [fastmail]", got)
	}
	// Folded into cfg.MCPStatic, fastmail is eager...
	cfgStatic := &config.Config{MCPStatic: []string{"fastmail"}}
	if got := resolveStaticMCP([]string{"fastmail", "notion"}, cfgStatic); len(got) != 1 || got[0] != "fastmail" {
		t.Errorf("pack-static fastmail must be eager, got %v", got)
	}
	// ...unless the user forces it dynamic.
	cfgOverride := &config.Config{MCPStatic: []string{"fastmail"}, MCPDynamic: []string{"fastmail"}}
	if got := resolveStaticMCP([]string{"fastmail", "notion"}, cfgOverride); len(got) != 0 {
		t.Errorf("mcp_dynamic must override pack-static, got %v", got)
	}
}

func TestBuildSbxArgs_DevBranch(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", Dev: true, DevRoot: "/repo", LocalKit: "/repo/pi-kit"}, "0.0.99")

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

// TestAnyModelKeyPresent + TestModelKeyMissingMessage (R1-1): the run bootstrap
// detects a present model key, and the guidance names the missing keys + the
// exact `sbx secret set` line when none is present.
func TestAnyModelKeyPresent(t *testing.T) {
	t.Run("no model key", func(t *testing.T) {
		f := fakeEnv{present: map[string]bool{"sbx": true}, output: map[string]string{"sbx secret ls": "github\n"}}
		if anyModelKeyPresent(f.env()) {
			t.Error("github alone is not a model key")
		}
	})
	t.Run("model key present", func(t *testing.T) {
		f := fakeEnv{present: map[string]bool{"sbx": true}, output: map[string]string{"sbx secret ls": "anthropic github\n"}}
		if !anyModelKeyPresent(f.env()) {
			t.Error("anthropic present should count")
		}
	})
	t.Run("sbx absent -> cannot verify (false)", func(t *testing.T) {
		f := fakeEnv{present: map[string]bool{}, output: map[string]string{}}
		if anyModelKeyPresent(f.env()) {
			t.Error("sbx absent must report not-present")
		}
	})
}

func TestModelKeyMissingMessage(t *testing.T) {
	f := fakeEnv{present: map[string]bool{"sbx": true}, output: map[string]string{"sbx secret ls": "github\n"}}
	msg := modelKeyMissingMessage(f.env())
	for _, want := range []string{"anthropic", "openai", "google", "pi-stack setup", "op://vault/item/field"} {
		if !strings.Contains(msg, want) {
			t.Errorf("guidance missing %q, got:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "sbx secret set -g") {
		t.Errorf("op is the only source now; guidance must not suggest a direct `sbx secret set`, got:\n%s", msg)
	}
}

// TestClassifyBareArg (R1-6): a path-like positional is diagnosed as a missing/
// !dir workspace (no such directory), while a bare-word typo gets the
// did-you-mean verb suggester.
func TestClassifyBareArg(t *testing.T) {
	t.Run("path-like -> no such directory", func(t *testing.T) {
		msg, launch := classifyBareArg("/tmp/missing-proj-does-not-exist")
		if launch {
			t.Fatal("a missing path must not launch run")
		}
		if !strings.Contains(msg, "no such directory") {
			t.Errorf("expected a no-such-directory message, got: %q", msg)
		}
		if strings.Contains(msg, "Did you mean") {
			t.Errorf("a path must NOT get a did-you-mean hint, got: %q", msg)
		}
	})

	t.Run("bare-word typo -> did you mean", func(t *testing.T) {
		msg, launch := classifyBareArg("memoyr")
		if launch {
			t.Fatal("a verb typo must not launch run")
		}
		if !strings.Contains(msg, "no command named") || !strings.Contains(msg, `Did you mean "memory"`) {
			t.Errorf("expected a did-you-mean memory hint, got: %q", msg)
		}
	})
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
