// hoststate.go builds the host-visible facts the fenced in-VM agent CANNOT see
// for itself (keys resolved, services up, gog/mcp state, models, pack) ENTIRELY
// IN MEMORY; run.go injects the JSON into the launcher-generated initial prompt.
//
// Why not a workspace file: <workspace>/.pix/host-state.json sits in a
// directory a user can `pix run` against after cloning an untrusted repo — a
// file there is racy (a stale copy, or a tracked file/symlink an attacker
// committed, can be read instead of a fresh write) and is plain content the
// agent reads like any other untrusted workspace file. Trusted facts travel
// only inside the launcher-generated prompt, which the agent treats specially.
// Nothing secret goes in it: booleans and names only, never a key value.
package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/rpc"
	"pix/host/secret"
	"pix/host/workflow/pack"
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
	Enabled bool `json:"enabled"`
	Up      bool `json:"up"`
	Port    int  `json:"port"`
}

// hostStateGog carries ONLY whether gog is wired, never the configured account
// email: `enabled` is all onboarding needs, and the address is real PII with no
// onboarding use that justifies putting it in every session's prompt. The
// account still lives in cfg.GogAccount for host-side use.
type hostStateGog struct {
	Enabled bool `json:"enabled"`
}

type hostStateMCP struct {
	Enabled bool     `json:"enabled"`
	Servers []string `json:"servers"`
}

type hostStateModels struct {
	Watcher string `json:"watcher"`
	Embed   string `json:"embed"`
}

type HostStatePack struct {
	Active         bool   `json:"active"`          // a pack is ACTUALLY active (config `pack` or a --pack override)
	Exists         bool   `json:"exists"`          // a loadable pack exists at Path
	Default        bool   `json:"default"`         // Path IS the default pack root
	Path           string `json:"path"`            // the active pack's root, or the default's when none is active
	GitInitialized bool   `json:"git_initialized"` // has a .git
	Skills         bool   `json:"skills"`          // has skills/
	Knowledge      bool   `json:"knowledge"`       // has knowledge/
}

// hostStateIdentity is who the user is, read from the HOST's git config (the
// sandbox cannot see ~/.gitconfig), so onboarding greets by FIRST name instead
// of starting anonymous. FIRST NAME ONLY — no surname, no email: this payload
// is injected into every session, so it carries the minimum PII needed to
// greet.
type hostStateIdentity struct {
	Name string `json:"name,omitempty"`
}

type HostState struct {
	Provisioned bool              `json:"provisioned"`
	Keys        hostStateKeys     `json:"keys"`
	Memory      hostStateSvc      `json:"memory"`
	Gog         hostStateGog      `json:"gog"`
	MCP         hostStateMCP      `json:"mcp"`
	Models      hostStateModels   `json:"models"`
	Pack        HostStatePack     `json:"pack"`
	Identity    hostStateIdentity `json:"identity"`
}

// ReadGitIdentity reads the user's FIRST name from git's GLOBAL config.
// --global (not repo-local) on purpose: a freshly cloned hostile repo can set a
// repo-local user.name to an injection payload, and it is cwd-independent so
// `pix setup /other/dir` still reads the right person. The value is UNTRUSTED
// display text — SanitizeIdentity reduces the injection surface, then only the
// leading token is kept. Email is deliberately not read. Best-effort: empty
// when git is absent or unset.
func ReadGitIdentity(env hostenv.Env) hostStateIdentity {
	id := hostStateIdentity{}
	if out, err := env.Run("git", "config", "--global", "--get", "user.name"); err == nil {
		if f := strings.Fields(SanitizeIdentity(out)); len(f) > 0 {
			id.Name = f[0]
		}
	}
	return id
}

// SanitizeIdentity reduces an untrusted git-config value to a single short line
// of GRAPHIC characters only: the first line is taken BEFORE trimming (so a
// leading blank line cannot promote line 2), then everything that is not
// unicode.IsGraphic is dropped — excluding C0/C1 controls, DEL, Cf format chars
// (ANSI ESC, C1 CSI U+009B, bidi overrides U+202E) and Zl/Zp separators
// (U+2028/U+2029). Capped by RUNE count so multibyte names are not truncated
// mid-rune or under-counted.
func SanitizeIdentity(s string) string {
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

// BuildHostState gathers the host-visible facts. Pure w.r.t. its inputs so it is
// unit-testable: sbxSecretsOut is the raw `sbx secret ls` output (sbxOK false
// when sbx could not be run), dial probes a local port.
func BuildHostState(cfg *config.Config, sbxSecretsOut string, sbxOK bool, dial func(int) bool, keysSource string, pack HostStatePack) HostState {
	dialer := func(p int) bool { return dial != nil && dial(p) }
	// A key is OK only when the probe ANSWERED and named it. An unreadable
	// `sbx secret ls` reports every key as not-set rather than inventing a
	// green: the payload is read by an agent that cannot check for itself.
	keyOK := func(name string) bool { return sbxOK && cli.GrepWord(sbxSecretsOut, name) }
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
	if !keys.Resolved && hasConfiguredKeylessModel(cfg) {
		keys.Resolved = true
		keys.Source = "configured inference"
	}

	mcpServers := append([]string(nil), cfg.MCP...)
	gogEnabled := slices.Contains(mcpServers, config.GWServerName)

	hs := HostState{
		Keys:   keys,
		Memory: hostStateSvc{Enabled: slices.Contains(cfg.Services, "memory"), Up: dialer(rpc.MemoryPortDefault), Port: rpc.MemoryPortDefault},
		Gog:    hostStateGog{Enabled: gogEnabled},
		MCP:    hostStateMCP{Enabled: len(mcpServers) > 0, Servers: mcpServers},
		Models: hostStateModels{Watcher: cfg.MemoryWatcherModel, Embed: cfg.MemoryEmbedModel},
		Pack:   pack,
	}
	// Provisioned: an inherited, fully set-up environment that must NOT be
	// re-onboarded — keys resolved AND a pack actually active.
	hs.Provisioned = keys.Resolved && hs.Pack.Active
	return hs
}

func hasConfiguredKeylessModel(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	for _, binding := range cfg.Inference.Models {
		if !binding.Available || !inference.Allowed(cfg, binding) {
			continue
		}
		backend, ok := cfg.Inference.Backends[binding.Backend]
		if ok && inference.BackendAllowed(cfg, backend, binding.Backend) && (backend.Auth == "sbx-session" || backend.Auth == "none") {
			return true
		}
	}
	return false
}

// ResolveHostStatePack reports pack truth for the in-VM onboarding agent.
// `Active` means ACTUALLY active: config `pack` (or a create-time --pack
// override) names a loadable pack. When neither is set the DEFAULT pack's facts
// are still reported but Active stays FALSE — the old code marked the default
// pack active merely because it existed on disk, which made the onboarding copy
// claim "a pack is active" on hosts where nothing was.
func ResolveHostStatePack(cfg *config.Config, override string) HostStatePack {
	root := pack.ActivePackRoot(cfg.Pack, override)
	active := root != ""
	if root == "" {
		root = pack.DefaultPackRoot() // runs the legacy pack/personal -> default migration
	}
	p, err := pack.LoadPack(root)
	if err != nil {
		return HostStatePack{}
	}
	_, gitErr := os.Stat(filepath.Join(root, ".git"))
	return HostStatePack{
		Active:         active,
		Exists:         true,
		Default:        pack.CanonicalizePackRoot(p.Root) == pack.CanonicalizePackRoot(pack.DefaultPackRoot()),
		Path:           p.Root,
		GitInitialized: gitErr == nil,
		Skills:         p.SkillsDir != "",
		Knowledge:      p.KnowledgeDir != "",
	}
}

// BuildTrustedHostState gathers the host-visible facts entirely in memory. Pure
// w.r.t. env/cfg (all I/O goes through the hostenv.Env seam) so it is
// unit-testable without touching disk.
func BuildTrustedHostState(cfg *config.Config, env hostenv.Env, packOverride string) HostState {
	sbxOut, sbxOK := "", false
	if _, err := env.LookPath("sbx"); err == nil {
		// BOUNDED: a hung `sbx secret ls` leaves sbxOK=false — keys stay
		// unverified and the payload build never wedges.
		if o, timedOut, rerr := env.RunTimed("sbx", "secret", "ls"); rerr == nil && !timedOut {
			sbxOut, sbxOK = o, true
		}
	}
	source := "sbx"
	if secret.ProviderKeyRefsPresent(env) {
		source = "1password"
	}
	hs := BuildHostState(cfg, sbxOut, sbxOK, env.DialLocal, source, ResolveHostStatePack(cfg, packOverride))
	hs.Identity = ReadGitIdentity(env)
	return hs
}

// EncodeTrustedHostState JSON-encodes hs for injection into the
// launcher-generated prompt. It is a separate function so the encode step has
// its own explicit error seam: InjectTrustedHostState must abort the launch
// BEFORE exec'ing sbx on a non-nil error, never proceed with a half-built or
// silently-omitted trusted payload.
func EncodeTrustedHostState(hs HostState) ([]byte, error) {
	b, err := json.Marshal(hs)
	if err != nil {
		return nil, fmt.Errorf("encoding trusted host state: %w", err)
	}
	return b, nil
}

// TrustedHostStateBegin/End delimit the trusted host-state JSON appended to the
// launcher-generated prompt, so the onboarding skill (and a human reading the
// transcript) can tell exactly where machine-generated data starts and ends.
// Keep this pair in sync with skills/onboarding/SKILL.md's parsing
// instructions.
const (
	TrustedHostStateBegin = "\n\n[pix-trusted-host-state]\n"
	TrustedHostStateEnd   = "\n[/pix-trusted-host-state]"
)

// InjectTrustedHostState appends the trusted host-state payload to the ONE pi
// passthrough arg that is the launcher's own generated prompt (prefixed with
// GeneratedInputMarker), and ONLY that arg, returning a COPY of args. This is
// the ENTIRE mechanism by which trusted host facts reach the fenced in-VM
// agent; an ordinary user-typed prompt never carries the marker.
//
// No marker (a normal `pix run`) is a no-op and runs NO host probe at all —
// onboarding truth is built only when there is a generated prompt to carry it.
// When the marker IS present, building/encoding the payload is a HARD contract:
// the caller aborts before exec'ing sbx rather than hand the onboarding agent a
// generated prompt with no trusted facts.
func InjectTrustedHostState(args []string, cfg *config.Config, env hostenv.Env, packOverride string) ([]string, error) {
	out := append([]string(nil), args...)
	idx := slices.IndexFunc(out, func(a string) bool { return strings.HasPrefix(a, GeneratedInputMarker) })
	if idx < 0 {
		return out, nil
	}
	b, err := EncodeTrustedHostState(BuildTrustedHostState(cfg, env, packOverride))
	if err != nil {
		return nil, err
	}
	out[idx] = out[idx] + TrustedHostStateBegin + string(b) + TrustedHostStateEnd
	return out, nil
}
