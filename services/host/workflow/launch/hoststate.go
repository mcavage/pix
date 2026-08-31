// hoststate.go builds the host-visible facts the fenced in-VM agent CANNOT see
// for itself (keys resolved, services up, mcp state, models, pack) ENTIRELY
// IN MEMORY; run.go injects the JSON into the launcher-generated initial prompt.
package launch

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/container"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/secret"
)

// memoryPort is the port THIS PIX_HOME's pix-memory container is published
// on. It is a per-home fact, not a constant: two PIX_HOMEs coexisting on one
// host each allocate their own loopback port (container.EnsureMemoryPort,
// persisted as config.toml's memory_port), so a hardcoded literal here dialed
// the OTHER stack's container — or nothing at all — and reported the answer
// as this stack's. cfg is the same machine config every other fact in this
// payload comes from; a home that has not run `pix setup` yet reads
// container.DefaultMemoryPort, the same "not allocated yet" value
// container.ReadMemoryPort returns.
func memoryPort(cfg *config.Config) int {
	if cfg != nil && cfg.MemoryPort != 0 {
		return cfg.MemoryPort
	}
	return container.DefaultMemoryPort
}

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

type hostStateMCP struct {
	Enabled bool     `json:"enabled"`
	Servers []string `json:"servers"`
}

type hostStateModels struct {
	Watcher string `json:"watcher"`
	Embed   string `json:"embed"`
}

type hostStateIdentity struct {
	Name string `json:"name,omitempty"`
}

type HostState struct {
	Provisioned bool              `json:"provisioned"`
	Keys        hostStateKeys     `json:"keys"`
	Memory      hostStateSvc      `json:"memory"`
	MCP         hostStateMCP      `json:"mcp"`
	Models      hostStateModels   `json:"models"`
	Identity    hostStateIdentity `json:"identity"`
}

func ReadGitIdentity(env hostenv.Env) hostStateIdentity {
	id := hostStateIdentity{}
	if out, err := env.Run("git", "config", "--global", "--get", "user.name"); err == nil {
		if f := strings.Fields(SanitizeIdentity(out)); len(f) > 0 {
			id.Name = f[0]
		}
	}
	return id
}

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

func BuildHostState(cfg *config.Config, sbxSecretsOut string, sbxOK bool, dial func(int) bool, keysSource string, githubGlobal bool) HostState {
	dialer := func(p int) bool { return dial != nil && dial(p) }
	// A key is OK only when the probe ANSWERED and named it. An unreadable
	// `sbx secret ls` reports every key as not-set rather than inventing a
	// green: the payload is read by an agent that cannot check for itself.
	keyOK := func(name string) bool { return sbxOK && cli.GrepWord(sbxSecretsOut, name) }
	// GitHub is asked a NARROWER question than the model keys, and the reason is
	// a real failure: `sbx secret ls` lists global and sandbox-scoped secrets
	// together, so a substring match reported github as available on a host where
	// it was pinned to one sandbox. The agent believed that, committed, and could
	// not push. A credential only one box can use is not a credential this
	// payload may promise.
	// githubGlobal is supplied by the caller, which asks sbx with its own
	// `--global` filter. Parsing the combined listing here would mean inventing a
	// literal for the SCOPE column, and the column is not ours to depend on.
	if keysSource == "" {
		keysSource = "sbx"
	}
	keys := hostStateKeys{
		Anthropic: keyOK("anthropic"),
		OpenAI:    keyOK("openai"),
		Google:    keyOK("google"),
		GitHub:    githubGlobal,
		Source:    keysSource,
	}
	keys.Resolved = keys.Anthropic || keys.OpenAI || keys.Google
	if !keys.Resolved && hasConfiguredKeylessModel(cfg) {
		keys.Resolved = true
		keys.Source = "configured inference"
	}

	hs := HostState{
		Keys: keys,
		// pix-memory is a reserved built-in (always declared), and MCP servers
		// are declared by the ENVIRONMENT's .sbxenv.yaml plus the reserved
		// built-ins — config.toml carries no second server list any more, so
		// this host-state summary reports none of its own.
		Memory: hostStateSvc{Enabled: true, Up: dialer(memoryPort(cfg)), Port: memoryPort(cfg)},
		MCP:    hostStateMCP{Enabled: false},
		Models: hostStateModels{Watcher: cfg.MemoryWatcherModel, Embed: cfg.MemoryEmbedModel},
	}
	// Provisioned: an inherited, fully set-up environment that must NOT be
	// re-onboarded — keys resolved.
	hs.Provisioned = keys.Resolved
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

func BuildTrustedHostState(cfg *config.Config, env hostenv.Env) HostState {
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
	// The github answer comes from THIS PIX_HOME's configured GITHUB_TOKEN
	// ref, because that is what actually reaches the box: every launch writes
	// it as a service secret scoped to the sandbox it is entering. A
	// host-global github secret is not this payload's evidence — the agent
	// reads this to decide whether it can push, and a credential Pix does not
	// give it is not one this payload may promise.
	ghState, _ := secret.ProbeGitHubCredential(env)
	hs := BuildHostState(cfg, sbxOut, sbxOK, env.DialLocal, source, ghState == secret.GitHubSecretGlobal)
	hs.Identity = ReadGitIdentity(env)
	return hs
}

func EncodeTrustedHostState(hs HostState) ([]byte, error) {
	b, err := json.Marshal(hs)
	if err != nil {
		return nil, fmt.Errorf("encoding trusted host state: %w", err)
	}
	return b, nil
}

const (
	TrustedHostStateBegin = "\n\n[pix-trusted-host-state]\n"
	TrustedHostStateEnd   = "\n[/pix-trusted-host-state]"
)

// InjectTrustedHostState appends the trusted host-state payload to the ONE pi
// passthrough arg that is the launcher's own generated prompt (prefixed with
// GeneratedInputMarker), and ONLY that arg, returning a COPY of args. This is
// the ENTIRE mechanism by which trusted host facts reach the fenced in-VM
// agent; an ordinary user-typed prompt never carries the marker.
func InjectTrustedHostState(args []string, cfg *config.Config, env hostenv.Env) ([]string, error) {
	out := append([]string(nil), args...)
	idx := slices.IndexFunc(out, func(a string) bool { return strings.HasPrefix(a, GeneratedInputMarker) })
	if idx < 0 {
		return out, nil
	}
	b, err := EncodeTrustedHostState(BuildTrustedHostState(cfg, env))
	if err != nil {
		return nil, err
	}
	out[idx] = out[idx] + TrustedHostStateBegin + string(b) + TrustedHostStateEnd
	return out, nil
}
