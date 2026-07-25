package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
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
	if !contains(args, []string{"--template", "docker.io/mcavage/pix:local-123"}) {
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
	want := "git+https://github.com/mcavage/pix.git#ref=main&dir=pi-kit"
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

func TestBuildSbxArgs_ExplicitTemplate(t *testing.T) {
	cfg := &config.Config{}
	ref := "docker.io/mcavage/pix:local-999"
	// Explicit --template with NO checkout context (the from-anywhere case): still
	// pins the image, and the kit falls back to the git pin as usual.
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", Template: ref}, "0.0.16+local")
	if !contains(args, []string{"--template", ref}) {
		t.Errorf("explicit --template should be pinned, got %v", args)
	}
	if countFlag(args, "--template") != 1 {
		t.Errorf("expected exactly one --template, got %v", args)
	}
}

func TestBuildSbxArgs_ExplicitTemplateBeatsLocalImageTag(t *testing.T) {
	cfg := &config.Config{}
	ref := "docker.io/mcavage/pix:local-999"
	// From a checkout that would auto-pin local-123, an explicit --template wins.
	args := buildSbxArgs(cfg, runOpts{
		Workspace:     ".",
		LocalKit:      "/repo/pi-kit",
		LocalImageTag: "local-123",
		Template:      ref,
	}, "0.0.16+local")
	if countFlag(args, "--template") != 1 {
		t.Fatalf("expected exactly one --template, got %v", args)
	}
	if !contains(args, []string{"--template", ref}) {
		t.Errorf("explicit --template should override the auto local-image pin, got %v", args)
	}
	if contains(args, []string{"--template", "docker.io/mcavage/pix:local-123"}) {
		t.Errorf("auto local-image pin should be suppressed by explicit --template, got %v", args)
	}
}

func TestBuildSbxArgs_TemplateOrthogonalToKit(t *testing.T) {
	cfg := &config.Config{}
	ref := "docker.io/mcavage/pix:local-999"
	// --kit replaces the SPEC, --template replaces the IMAGE: both apply together.
	args := buildSbxArgs(cfg, runOpts{
		Workspace: ".",
		Kits:      []string{"/my/kit"},
		Template:  ref,
	}, "0.0.16+local")
	if !contains(args, []string{"--kit", "/my/kit"}) {
		t.Errorf("--kit override missing, got %v", args)
	}
	if !contains(args, []string{"--template", ref}) {
		t.Errorf("--template should survive alongside --kit (orthogonal), got %v", args)
	}
}

func TestParseRunArgs_Template(t *testing.T) {
	ref := "docker.io/mcavage/pix:local-999"
	o, err := parseRunArgs([]string{"--template", ref, "--replace"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.Template != ref {
		t.Errorf("Template = %q, want %q", o.Template, ref)
	}
	if !o.Replace {
		t.Errorf("Replace should be set")
	}
	// --template=REF form too.
	o2, err := parseRunArgs([]string{"--template=" + ref})
	if err != nil {
		t.Fatalf("parse (=form): %v", err)
	}
	if o2.Template != ref {
		t.Errorf("=form Template = %q, want %q", o2.Template, ref)
	}
}

func TestTemplateTag(t *testing.T) {
	cases := map[string]string{
		"docker.io/mcavage/pix:local-999": "local-999",
		"docker.io/mcavage/pix:0.0.46":    "0.0.46",
		"localhost:5000/foo:local-1":      "local-1", // registry port must not confuse it
		"docker.io/mcavage/pix":           "",        // no tag
		"localhost:5000/foo":              "",        // port only, no tag
	}
	for ref, want := range cases {
		if got := templateTag(ref); got != want {
			t.Errorf("templateTag(%q) = %q, want %q", ref, got, want)
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
	vtag := "git+https://github.com/mcavage/pix.git#ref=v0.0.16&dir=pi-kit"
	msg := kitResolveFailureMsg(pinnedGitKit([]string{"--kit", vtag}))
	if !strings.Contains(msg, "v0.0.16") || !strings.Contains(msg, "--dev") || !strings.Contains(msg, "--kit") {
		t.Errorf("v-tag failure message missing key hints: %q", msg)
	}
	// A main pin still explains.
	mainRef := "git+https://github.com/mcavage/pix.git#ref=main&dir=pi-kit"
	if msg := kitResolveFailureMsg(pinnedGitKit([]string{"--kit", mainRef})); !strings.Contains(msg, "main") {
		t.Errorf("main failure message should mention the ref, got %q", msg)
	}
}

func TestBuildSbxArgs_VersionPin(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: "."}, "0.0.99")

	if args[0] != "run" || args[1] != "pix" {
		t.Fatalf("args should start with `run pix`, got %v", args[:2])
	}
	want := "git+https://github.com/mcavage/pix.git#ref=v0.0.99&dir=pi-kit"
	if !contains(args, []string{"--kit", want}) {
		t.Errorf("expected pinned kit %q in %v", want, args)
	}
}

func TestBuildSbxArgs_DevVersionTracksMain(t *testing.T) {
	cfg := &config.Config{}
	args := buildSbxArgs(cfg, runOpts{Workspace: "."}, "dev")

	want := "git+https://github.com/mcavage/pix.git#ref=main&dir=pi-kit"
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
	cfg.Kits.Stack = []string{"/mixin/kit", "git+https://example.com/other#dir=kit"}
	args := buildSbxArgs(cfg, runOpts{Workspace: ".", Kits: []string{"/flag/kit"}}, "0.0.99")

	// --kit override (1 flag kit, base) + 2 config stack = 3 --kit total. The
	// override suppresses the auto git pin.
	if got := countFlag(args, "--kit"); got != 3 {
		t.Errorf("expected 3 --kit flags, got %d in %v", got, args)
	}
	if !contains(args, []string{"--kit", "/mixin/kit"}) {
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
	cfg.Kits.Stack = []string{"/mixin/kit"}
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
	// buildSbxArgs emits --static-mcp for the PRELOADED set (o.StaticMCP); the
	// caller computes it via allPreloadedMCP. The sbx local gateway serves them,
	// no SBX_MCP_URL.
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

// allPreloadedMCP: S01 — every configured server preloads, no eager/lazy
// split. It is pure list hygiene: dedupe + drop empties, order preserved.
func TestAllPreloadedMCP(t *testing.T) {
	if got := allPreloadedMCP(nil); len(got) != 0 {
		t.Errorf("allPreloadedMCP(nil) = %v, want none", got)
	}
	got := allPreloadedMCP([]string{"gog", "slack", "notion", "slack", "", "atlassian"})
	want := []string{"gog", "slack", "notion", "atlassian"}
	if len(got) != len(want) {
		t.Fatalf("allPreloadedMCP = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("allPreloadedMCP = %v, want %v", got, want)
		}
	}
}

// TestApplyPackToLaunch_IntegrationMCPAlwaysPreloaded: S01 removed the
// per-integration `static` field — every pack integration's MCP server is now
// in the preload set unconditionally. A --pack OVERRIDE (never `pack use`d, so
// its integration is not yet in cfg.MCP) still gets folded in, in memory only,
// for this launch.
func TestApplyPackToLaunch_IntegrationMCPAlwaysPreloaded(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "override-pack")
	mustWritePack(t, root, packManifest{Name: "override", Schema: 1, Integrations: []packIntegration{
		{Name: "Fastmail", MCP: "fastmail"},
		{Name: "Notion", MCP: "notion"},
		{Name: "NoServer"}, // no mcp -> ignored
	}})

	cfgPath := filepath.Join(dir, "config.toml")
	const before = "mcp = [\"existing\"]\n"
	if err := os.WriteFile(cfgPath, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", cfgPath)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	o := runOpts{Pack: root}
	if _, err := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("applyPackToLaunch: %v", err)
	}
	if !containsStr(cfg.MCP, "fastmail") || !containsStr(cfg.MCP, "notion") {
		t.Errorf("cfg.MCP = %v, want it to contain both integration servers (every pack integration preloads)", cfg.MCP)
	}
	if got := allPreloadedMCP(cfg.MCP); len(got) != len(cfg.MCP) {
		t.Errorf("every entry in cfg.MCP should be in the preload set, got %v vs %v", got, cfg.MCP)
	}

	// Never persisted: applyPackToLaunch must not itself write config.toml —
	// run.go/task.go never call cfg.Save() after it, so a --pack override's
	// integration names must not have reached disk.
	onDisk, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(onDisk) != before {
		t.Errorf("config.toml changed on disk after applyPackToLaunch, want unchanged %q, got %q", before, onDisk)
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
	for _, want := range []string{"anthropic", "openai", "google", "pix setup", "op://vault/item/field"} {
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
