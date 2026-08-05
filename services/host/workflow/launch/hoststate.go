// hoststate.go builds the host-visible facts the fenced in-VM agent CANNOT see
// for itself (keys resolved, services up, gog/mcp state, models,
// pack/provisioned) ENTIRELY IN MEMORY — it is never written to a
// workspace file. run.go injects the resulting JSON directly into the
// launcher-generated initial prompt (the one message carrying
// GeneratedInputMarker), so the onboarding skill reads trusted facts from that
// prompt instead of guessing, and instead of reading anything from the
// (attacker-influenced) workspace.
//
// Why not a file: a workspace like <workspace>/.pix/host-state.json is
// inside a directory a user can `pix run` against after cloning an
// untrusted repo. A file there is racy (nothing stops a stale copy from a
// prior run, or a tracked file/symlink an attacker committed, from being read
// instead of — or before — a fresh write) and is plain file content the agent
// would read like any other untrusted workspace file. Trusted facts must
// travel only inside the launcher-generated prompt, which the agent already
// treats specially (see GeneratedInputMarker in setup.go), never through a
// path a hostile clone can also write to.
//
// Nothing secret goes in it: booleans and names only, never a key value.
package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/secret"
	"pix/host/workflow/pack"
	"slices"
	"strings"
	"unicode"

	"pix/host/config"
	"pix/host/inference"
	"pix/host/rpc"
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

// hostStateGog carries ONLY whether gog is wired, never the configured
// account email. `enabled` is sufficient for onboarding to say "Gmail/Drive
// isn't set up yet" or "it's on" — the email address is real PII with no
// onboarding use that justifies putting it in every session's prompt. The
// account still lives in cfg.GogAccount (host config, doctor, `gog setup`
// re-registration) — it is just never copied into this model-visible
// payload. Same rationale as hostStateIdentity dropping email.
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
	Path           string `json:"path"`            // the active pack's root, or the default pack's when none is active
	GitInitialized bool   `json:"git_initialized"` // has a .git
	Skills         bool   `json:"skills"`          // has skills/
	Knowledge      bool   `json:"knowledge"`       // has knowledge/
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

// ReadGitIdentity reads the user's FIRST name from git's GLOBAL config.
// --global (not repo-local) on purpose: a freshly cloned hostile repo can set a
// repo-local user.name to an injection payload, and it's cwd-independent so
// `pix setup /other/dir` still reads the right person. The value is
// UNTRUSTED display text — SanitizeIdentity reduces the injection surface (strips
// terminal-control/format chars, caps length), then firstName takes only the
// leading token. Email is deliberately NOT read (unused, and it's PII we won't
// inject). Best-effort: empty when git is absent or unset.
func ReadGitIdentity(env hostenv.Env) hostStateIdentity {
	id := hostStateIdentity{}

	if out, err := env.Run("git", "config", "--global", "--get", "user.name"); err == nil {
		id.Name = firstName(SanitizeIdentity(out))
	}
	return id
}

// SanitizeIdentity reduces an untrusted git-config value to a single short line
// of GRAPHIC characters only: first line taken BEFORE trimming (so a leading
// blank line can't promote line 2), then everything that isn't unicode.IsGraphic
// is dropped — that excludes C0/C1 controls, DEL, Cf format chars (ANSI ESC, C1
// CSI U+009B, bidi overrides U+202E), and Zl/Zp line/paragraph separators
// (U+2028/U+2029). Capped by RUNE count (not bytes) so multibyte names aren't
// truncated mid-rune or under-counted.
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
// when sbx couldn't be run), dial probes a local port.
func BuildHostState(cfg *config.Config, sbxSecretsOut string, sbxOK bool, dial func(int) bool, keysSource string, pack HostStatePack) HostState {
	dialer := func(p int) bool {
		if dial == nil {
			return false
		}
		return dial(p)
	}
	// A key is OK only when the probe ANSWERED and named it. An unreadable
	// `sbx secret ls` (sbxOK false, e.g. run from inside the sandbox) reports
	// every key as not-set rather than inventing a green: the payload is read
	// by an agent that cannot check for itself.
	keyOK := func(name string) bool {
		return sbxOK && cli.GrepWord(sbxSecretsOut, name)
	}
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
	gogEnabled := false
	for _, m := range mcpServers {
		if strings.TrimSpace(m) == config.GWServerName {
			gogEnabled = true
		}
	}

	hs := HostState{
		Keys:   keys,
		Memory: hostStateSvc{Enabled: slices.Contains(cfg.Services, "memory"), Up: dialer(rpc.MemoryPortDefault), Port: rpc.MemoryPortDefault},
		Gog:    hostStateGog{Enabled: gogEnabled},
		MCP:    hostStateMCP{Enabled: len(mcpServers) > 0, Servers: mcpServers},
		Models: hostStateModels{Watcher: cfg.MemoryWatcherModel, Embed: cfg.MemoryEmbedModel},
		Pack:   pack,
	}
	// Provisioned: an inherited, fully set-up environment that must NOT be
	// re-onboarded — keys resolved AND a pack actually active. Onboarding
	// short-circuits to "you're set up" on true.
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

// ResolveHostStatePack reports pack truth for the in-VM onboarding agent, so
// it states facts instead of guessing. `Active` means ACTUALLY active: config
// `pack` (or a create-time --pack override) names a loadable pack. When
// neither is set, the DEFAULT pack's existence and facts are still reported
// (Exists/Default/Path/...), but Active stays FALSE — the old code marked the
// default pack active merely because it existed on disk, which made the
// onboarding copy unconditionally claim "a pack is active" on hosts where
// nothing was.
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
	gitInit := false
	if _, e := os.Stat(filepath.Join(root, ".git")); e == nil {
		gitInit = true
	}
	return HostStatePack{
		Active:         active,
		Exists:         true,
		Default:        pack.CanonicalizePackRoot(p.Root) == pack.CanonicalizePackRoot(pack.DefaultPackRoot()),
		Path:           p.Root,
		GitInitialized: gitInit,
		Skills:         p.SkillsDir != "",
		Knowledge:      p.KnowledgeDir != "",
	}
}

// BuildTrustedHostState gathers the host-visible facts, entirely in memory —
// reusing the exact same probes (sbx secret ls, port dial, key-ref source,
// pack resolution, git identity) writeHostStateFile used to run before it
// wrote them to a file. Pure w.r.t. env/cfg (all I/O goes through the hostenv.Env
// seam), so it is unit-testable without touching disk.
func BuildTrustedHostState(cfg *config.Config, env hostenv.Env, packOverride string) HostState {
	sbxOut, sbxOK := "", false
	if _, err := env.LookPath("sbx"); err == nil {
		// BOUNDED (probeRun): a hung `sbx secret ls` leaves sbxOK=false —
		// keys stay unverified, and setup's payload build never wedges.
		if o, timedOut, rerr := env.RunTimed("sbx", "secret", "ls"); rerr == nil && !timedOut {
			sbxOut, sbxOK = o, true
		}
	}
	dial := env.DialLocal
	source := "sbx"
	if secret.ProviderKeyRefsPresent(env) {
		source = "1password"
	}
	hs := BuildHostState(cfg, sbxOut, sbxOK, dial, source, ResolveHostStatePack(cfg, packOverride))
	hs.Identity = ReadGitIdentity(env)
	return hs
}

// EncodeTrustedHostState JSON-encodes hs for injection into the
// launcher-generated initial prompt. A separate function (rather than
// inlining json.Marshal at the call site) so the encode step has its own
// explicit error seam: the caller (InjectTrustedHostState) must abort the
// launch BEFORE exec'ing sbx on a non-nil error, never proceed with a
// half-built or silently-omitted trusted payload.
func EncodeTrustedHostState(hs HostState) ([]byte, error) {
	b, err := json.Marshal(hs)
	if err != nil {
		return nil, fmt.Errorf("encoding trusted host state: %w", err)
	}
	return b, nil
}

// TrustedHostStateBegin/End clearly delimit the trusted host-state JSON block
// appended to the launcher-generated prompt, so the onboarding skill (and a
// human glancing at the transcript) can tell exactly where machine-generated
// data starts and ends inside that one message. Keep this pair in sync with
// skills/onboarding/SKILL.md's parsing instructions.
const (
	TrustedHostStateBegin = "\n\n[pix-trusted-host-state]\n"
	TrustedHostStateEnd   = "\n[/pix-trusted-host-state]"
)

// InjectTrustedHostState appends the trusted host-state JSON payload to the
// ONE pi passthrough arg that is the launcher's own generated prompt (the arg
// with GeneratedInputMarker as a prefix — see setup.go), and ONLY that arg.
// It returns a COPY of args; the input slice is never mutated.
//
// This is the ENTIRE mechanism by which trusted host facts reach the fenced
// in-VM agent: no file is written to the workspace for this purpose. An
// ordinary user-typed prompt never carries GeneratedInputMarker, so it is
// never a target here — InjectTrustedHostState must not, and does not, touch
// any arg the user actually typed.
//
// When no generated-marker arg is present (a normal `pix run` with no
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
func InjectTrustedHostState(args []string, cfg *config.Config, env hostenv.Env, packOverride string) ([]string, error) {
	idx := -1
	for i, a := range args {
		if strings.HasPrefix(a, GeneratedInputMarker) {
			idx = i
			break
		}
	}
	out := append([]string(nil), args...)
	if idx < 0 {
		return out, nil
	}
	hs := BuildTrustedHostState(cfg, env, packOverride)
	b, err := EncodeTrustedHostState(hs)
	if err != nil {
		return nil, err
	}
	out[idx] = out[idx] + TrustedHostStateBegin + string(b) + TrustedHostStateEnd
	return out, nil
}
