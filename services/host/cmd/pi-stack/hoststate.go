// hoststate.go writes <workspace>/.pi-stack/host-state.json at launch: the
// host-visible facts the fenced in-VM agent CANNOT see for itself (keys resolved,
// services up, knowledge bundles, gog/mcp state, models, overlay/provisioned).
// The onboarding skill READS this and states facts instead of guessing — the fix
// for "the agent said 'no knowledge base' when one was configured".
//
// Nothing secret goes in it: booleans and names only, never a key value.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

type hostStateKeys struct {
	Anthropic bool   `json:"anthropic"`
	OpenAI    bool   `json:"openai"`
	Google    bool   `json:"google"`
	GitHub    bool   `json:"github"`
	Resolved  bool   `json:"resolved"` // at least one MODEL key present (github doesn't count)
	Source    string `json:"source"`   // where keys come from: "sbx" (op:// refs land here later)
}

type hostStateSvc struct {
	Up   bool `json:"up"`
	Port int  `json:"port"`
}

type hostStateKnowledge struct {
	Bundles   []string `json:"bundles"`
	Seeded    bool     `json:"seeded"`
	ServiceUp bool     `json:"service_up"`
}

type hostStateGog struct {
	Enabled bool   `json:"enabled"`
	Account string `json:"account"`
}

type hostStateMCP struct {
	Enabled bool     `json:"enabled"`
	Servers []string `json:"servers"`
}

type hostStateOverlay struct {
	Kit string `json:"kit"`
}

type hostStateModels struct {
	Watcher string `json:"watcher"`
	Embed   string `json:"embed"`
}

type hostStateHost struct {
	Enabled     bool `json:"enabled"`     // host.enabled config gate
	Provisioned bool `json:"provisioned"` // host agent dir actually set up
	Ready       bool `json:"ready"`       // enabled AND provisioned (safe to claim)
}

type hostStatePack struct {
	Active         bool   `json:"active"`          // an active/personal pack exists
	Path           string `json:"path"`            // its root
	GitInitialized bool   `json:"git_initialized"` // has a .git
	Skills         bool   `json:"skills"`          // has skills/
	Knowledge      bool   `json:"knowledge"`       // has knowledge/
}

type hostState struct {
	Provisioned bool               `json:"provisioned"`
	Keys        hostStateKeys      `json:"keys"`
	Memory      hostStateSvc       `json:"memory"`
	Knowledge   hostStateKnowledge `json:"knowledge"`
	Gog         hostStateGog       `json:"gog"`
	MCP         hostStateMCP       `json:"mcp"`
	Overlay     hostStateOverlay   `json:"overlay"`
	Models      hostStateModels    `json:"models"`
	Pack        hostStatePack      `json:"pack"`
	Host        hostStateHost      `json:"host"`
}

// buildHostState gathers the host-visible facts. Pure w.r.t. its inputs so it is
// unit-testable: sbxSecretsOut is the raw `sbx secret ls` output (sbxOK false
// when sbx couldn't be run), dial probes a local port.
func buildHostState(cfg *config.Config, sbxSecretsOut string, sbxOK bool, dial func(int) bool, mcpGatewayOn bool, keysSource string, pack hostStatePack) hostState {
	dialer := func(p int) bool {
		if dial == nil {
			return false
		}
		return dial(p)
	}
	keyOK := func(name string) bool { return secretCheck(name, name, sbxSecretsOut, sbxOK).state == stateOK }
	if keysSource == "" {
		keysSource = "sbx"
	}
	keys := hostStateKeys{
		Anthropic: keyOK("anthropic"),
		OpenAI:    keyOK("openai"),
		Google:    keyOK("google"),
		GitHub:    keyOK("github"),
		Source:    keysSource,
	}
	keys.Resolved = keys.Anthropic || keys.OpenAI || keys.Google

	bundles := append([]string(nil), cfg.KnowledgeBundles...)
	overlayKit := ""
	if stack := cfg.Kits.Stack; len(stack) > 0 {
		overlayKit = filepath.Base(strings.TrimSpace(stack[0]))
	}

	mcpServers := append([]string(nil), cfg.MCP...)
	gogEnabled := false
	for _, m := range mcpServers {
		if strings.TrimSpace(m) == "gog" {
			gogEnabled = true
		}
	}

	hs := hostState{
		Keys:      keys,
		Memory:    hostStateSvc{Up: dialer(memoryPortDefault), Port: memoryPortDefault},
		Knowledge: hostStateKnowledge{Bundles: bundles, Seeded: len(bundles) > 0, ServiceUp: dialer(knowledgePortDefault)},
		Gog:       hostStateGog{Enabled: gogEnabled, Account: cfg.GogAccount},
		MCP:       hostStateMCP{Enabled: mcpGatewayOn && len(mcpServers) > 0, Servers: mcpServers},
		Overlay:   hostStateOverlay{Kit: overlayKit},
		Models:    hostStateModels{Watcher: cfg.MemoryWatcherModel, Embed: cfg.MemoryEmbedModel},
		Pack:      pack,
		Host:      buildHostStateHost(cfg),
	}
	// Provisioned: an inherited, fully set-up environment that must NOT be
	// re-onboarded — keys resolved AND a knowledge bundle already seeded AND an
	// overlay kit stacked. Onboarding short-circuits to "you're set up" on true.
	hs.Provisioned = keys.Resolved && hs.Knowledge.Seeded && overlayKit != ""
	return hs
}

// containsStr reports whether list contains s.
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// dirHasEntries reports whether path is a directory with at least one entry.
func dirHasEntries(path string) bool {
	ents, err := os.ReadDir(path)
	return err == nil && len(ents) > 0
}

// buildHostStateHost builds the host slice with a SINGLE hostProvisioned() probe so
// Ready can never disagree with Provisioned within one snapshot.
func buildHostStateHost(cfg *config.Config) hostStateHost {
	prov := hostProvisioned()
	return hostStateHost{Enabled: cfg.Host.Enabled, Provisioned: prov, Ready: cfg.Host.Enabled && prov}
}

// hostProvisioned reports whether host mode is actually installed (the agent dir
// has settings.json AND the host-guard extension), mirroring runHostLaunch's own
// launch preconditions — so host-state never claims "ready" for a bare
// host.enabled flag with nothing behind it.
func hostProvisioned() bool {
	dir := hostAgentDir()
	if _, err := os.Stat(filepath.Join(dir, "settings.json")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "extensions", "host-guard.ts")); err != nil {
		return false
	}
	// host mode launches `pi` DIRECTLY on the host; without it, host mode can't
	// run, so it isn't truly provisioned (don't let host-state claim "ready").
	if _, err := exec.LookPath("pi"); err != nil {
		return false
	}
	return true
}

// resolveHostStatePack reports the active pack (config `pack`) or, failing that,
// the personal pack (PackDir), so the in-VM onboarding agent states pack facts
// instead of guessing.
func resolveHostStatePack(cfg *config.Config, override string) hostStatePack {
	root := activePackRoot(cfg.Pack, override)
	if root == "" {
		root = config.PackDir()
	}
	p, err := loadPack(root)
	if err != nil {
		return hostStatePack{}
	}
	gitInit := false
	if _, e := os.Stat(filepath.Join(root, ".git")); e == nil {
		gitInit = true
	}
	return hostStatePack{
		Active:         true,
		Path:           p.Root,
		GitInitialized: gitInit,
		Skills:         p.SkillsDir != "",
		Knowledge:      p.KnowledgeDir != "",
	}
}

// writeHostStateFile writes <workspace>/.pi-stack/host-state.json. Best-effort:
// a failure just means the agent has no truth file and falls back to probing
// (the pre-fix behavior), never a broken launch.
func writeHostStateFile(workspace string, cfg *config.Config, env shellEnv, mcpGatewayOn bool, packOverride string) {
	sbxOut, sbxOK := "", false
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil && env.run != nil {
			if o, rerr := env.run("sbx", "secret", "ls"); rerr == nil {
				sbxOut, sbxOK = o, true
			}
		}
	}
	dial := env.dial
	if dial == nil {
		dial = dialLocalPort
	}
	source := "sbx"
	if providerKeyRefsPresent(env) {
		source = "1password"
	}
	hs := buildHostState(cfg, sbxOut, sbxOK, dial, mcpGatewayOn, source, resolveHostStatePack(cfg, packOverride))

	dir := filepath.Join(workspace, ".pi-stack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	b, err := json.MarshalIndent(hs, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "host-state.json"), append(b, '\n'), 0o644)
}
