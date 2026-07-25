// hoststate.go builds the host-visible facts the fenced in-VM agent CANNOT see
// for itself (keys resolved, services up, knowledge bundles, gog/mcp state,
// models, pack/provisioned) ENTIRELY IN MEMORY — it is never written to a
// workspace file. run.go injects the resulting JSON directly into the
// launcher-generated initial prompt (the one message carrying
// generatedInputMarker), so the onboarding skill reads trusted facts from that
// prompt instead of guessing, and instead of reading anything from the
// (attacker-influenced) workspace.
//
// Why not a file: a workspace like <workspace>/.pi-stack/host-state.json is
// inside a directory a user can `pi-stack run` against after cloning an
// untrusted repo. A file there is racy (nothing stops a stale copy from a
// prior run, or a tracked file/symlink an attacker committed, from being read
// instead of — or before — a fresh write) and is plain file content the agent
// would read like any other untrusted workspace file. Trusted facts must
// travel only inside the launcher-generated prompt, which the agent already
// treats specially (see generatedInputMarker in setup.go), never through a
// path a hostile clone can also write to.
//
// Nothing secret goes in it: booleans and names only, never a key value.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

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
	Active         bool   `json:"active"`          // a pack is ACTUALLY active (config `pack` or a --pack override)
	Exists         bool   `json:"exists"`          // a loadable pack exists at Path
	Default        bool   `json:"default"`         // Path IS the default pack root
	Path           string `json:"path"`            // the active pack's root, or the default pack's when none is active
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
	Models      hostStateModels    `json:"models"`
	Pack        hostStatePack      `json:"pack"`
	Host        hostStateHost      `json:"host"`
	Identity    hostStateIdentity  `json:"identity"`
}

// hostStateIdentity is who the user is, read from the HOST's git config (the
// sandbox can't see ~/.gitconfig), so onboarding can greet them by FIRST name
// instead of starting anonymous. Deliberately FIRST NAME ONLY — no surname, no
// email: this payload is injected into every session, so it carries the minimum
// PII needed to greet, nothing a model should be handed by default.
type hostStateIdentity struct {
	Name string `json:"name,omitempty"` // first name only
}

// firstName returns the first whitespace-delimited token of a name, or "".
func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return ""
}

// readGitIdentity reads the user's FIRST name from git's GLOBAL config.
// --global (not repo-local) on purpose: a freshly cloned hostile repo can set a
// repo-local user.name to an injection payload, and it's cwd-independent so
// `pi-stack setup /other/dir` still reads the right person. The value is
// UNTRUSTED display text — sanitizeIdentity reduces the injection surface (strips
// terminal-control/format chars, caps length), then firstName takes only the
// leading token. Email is deliberately NOT read (unused, and it's PII we won't
// inject). Best-effort: empty when git is absent or unset.
func readGitIdentity(env shellEnv) hostStateIdentity {
	id := hostStateIdentity{}
	if env.run == nil {
		return id
	}
	if out, err := env.run("git", "config", "--global", "--get", "user.name"); err == nil {
		id.Name = firstName(sanitizeIdentity(out))
	}
	return id
}

// sanitizeIdentity reduces an untrusted git-config value to a single short line
// of GRAPHIC characters only: first line taken BEFORE trimming (so a leading
// blank line can't promote line 2), then everything that isn't unicode.IsGraphic
// is dropped — that excludes C0/C1 controls, DEL, Cf format chars (ANSI ESC, C1
// CSI U+009B, bidi overrides U+202E), and Zl/Zp line/paragraph separators
// (U+2028/U+2029). Capped by RUNE count (not bytes) so multibyte names aren't
// truncated mid-rune or under-counted.
func sanitizeIdentity(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if !unicode.IsGraphic(r) {
			continue
		}
		b.WriteRune(r)
		if n++; n >= 60 {
			break
		}
	}
	return strings.TrimSpace(b.String())
}

// buildHostState gathers the host-visible facts. Pure w.r.t. its inputs so it is
// unit-testable: sbxSecretsOut is the raw `sbx secret ls` output (sbxOK false
// when sbx couldn't be run), dial probes a local port.
func buildHostState(cfg *config.Config, sbxSecretsOut string, sbxOK bool, dial func(int) bool, keysSource string, pack hostStatePack) hostState {
	dialer := func(p int) bool {
		if dial == nil {
			return false
		}
		return dial(p)
	}
	keyOK := func(name string) bool { return secretCheck(name, name, sbxSecretsOut, sbxOK).state() == stateOK }
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
		MCP:       hostStateMCP{Enabled: len(mcpServers) > 0, Servers: mcpServers},
		Models:    hostStateModels{Watcher: cfg.MemoryWatcherModel, Embed: cfg.MemoryEmbedModel},
		Pack:      pack,
		Host:      buildHostStateHost(cfg),
	}
	// Provisioned: an inherited, fully set-up environment that must NOT be
	// re-onboarded — keys resolved AND a knowledge bundle already seeded AND a
	// pack actually active. Onboarding short-circuits to "you're set up" on true.
	hs.Provisioned = keys.Resolved && hs.Knowledge.Seeded && hs.Pack.Active
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

// resolveHostStatePack reports pack truth for the in-VM onboarding agent, so
// it states facts instead of guessing. `Active` means ACTUALLY active: config
// `pack` (or a create-time --pack override) names a loadable pack. When
// neither is set, the DEFAULT pack's existence and facts are still reported
// (Exists/Default/Path/...), but Active stays FALSE — the old code marked the
// default pack active merely because it existed on disk, which made the
// onboarding copy unconditionally claim "a pack is active" on hosts where
// nothing was.
func resolveHostStatePack(cfg *config.Config, override string) hostStatePack {
	root := activePackRoot(cfg.Pack, override)
	active := root != ""
	if root == "" {
		root = defaultPackRoot() // runs the legacy pack/personal -> default migration
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
		Active:         active,
		Exists:         true,
		Default:        canonicalizePackRoot(p.Root) == canonicalizePackRoot(defaultPackRoot()),
		Path:           p.Root,
		GitInitialized: gitInit,
		Skills:         p.SkillsDir != "",
		Knowledge:      p.KnowledgeDir != "",
	}
}

// buildTrustedHostState gathers the host-visible facts, entirely in memory —
// reusing the exact same probes (sbx secret ls, port dial, key-ref source,
// pack resolution, git identity) writeHostStateFile used to run before it
// wrote them to a file. Pure w.r.t. env/cfg (all I/O goes through the shellEnv
// seam), so it is unit-testable without touching disk.
func buildTrustedHostState(cfg *config.Config, env shellEnv, packOverride string) hostState {
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
	hs := buildHostState(cfg, sbxOut, sbxOK, dial, source, resolveHostStatePack(cfg, packOverride))
	hs.Identity = readGitIdentity(env)
	return hs
}

// encodeTrustedHostState JSON-encodes hs for injection into the
// launcher-generated initial prompt. A separate function (rather than
// inlining json.Marshal at the call site) so the encode step has its own
// explicit error seam: the caller (injectTrustedHostState) must abort the
// launch BEFORE exec'ing sbx on a non-nil error, never proceed with a
// half-built or silently-omitted trusted payload.
func encodeTrustedHostState(hs hostState) ([]byte, error) {
	b, err := json.Marshal(hs)
	if err != nil {
		return nil, fmt.Errorf("encoding trusted host state: %w", err)
	}
	return b, nil
}

// trustedHostStateBegin/End clearly delimit the trusted host-state JSON block
// appended to the launcher-generated prompt, so the onboarding skill (and a
// human glancing at the transcript) can tell exactly where machine-generated
// data starts and ends inside that one message. Keep this pair in sync with
// skills/onboarding/SKILL.md's parsing instructions.
const (
	trustedHostStateBegin = "\n\n[pi-stack-trusted-host-state]\n"
	trustedHostStateEnd   = "\n[/pi-stack-trusted-host-state]"
)

// injectTrustedHostState appends the trusted host-state JSON payload to the
// ONE pi passthrough arg that is the launcher's own generated prompt (the arg
// with generatedInputMarker as a prefix — see setup.go), and ONLY that arg.
// It returns a COPY of args; the input slice is never mutated.
//
// This is the ENTIRE mechanism by which trusted host facts reach the fenced
// in-VM agent: no file is written to the workspace for this purpose. An
// ordinary user-typed prompt never carries generatedInputMarker, so it is
// never a target here — injectTrustedHostState must not, and does not, touch
// any arg the user actually typed.
//
// When no generated-marker arg is present (a normal `pi-stack run` with no
// onboarding kickoff), this is a no-op: args is copied unchanged and no host
// probe (sbx secret ls, port dial, git config, pack resolution) runs at all —
// onboarding truth is built ONLY when there is actually a generated prompt to
// carry it.
//
// Building or encoding the trusted state is a HARD contract when a
// generated-marker arg IS present: a launch that can't produce the payload
// must abort BEFORE exec'ing sbx (the caller in run.go checks the returned
// error) rather than hand the onboarding agent a generated prompt with no
// trusted facts, or — worse — let it fall back to reading something else.
func injectTrustedHostState(args []string, cfg *config.Config, env shellEnv, packOverride string) ([]string, error) {
	idx := -1
	for i, a := range args {
		if strings.HasPrefix(a, generatedInputMarker) {
			idx = i
			break
		}
	}
	out := append([]string(nil), args...)
	if idx < 0 {
		return out, nil
	}
	hs := buildTrustedHostState(cfg, env, packOverride)
	b, err := encodeTrustedHostState(hs)
	if err != nil {
		return nil, err
	}
	out[idx] = out[idx] + trustedHostStateBegin + string(b) + trustedHostStateEnd
	return out, nil
}
