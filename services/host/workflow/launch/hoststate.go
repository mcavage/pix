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
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/packinfo"
	"pix/host/rpc"
	"pix/host/secret"
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
	Pack        packinfo.State    `json:"pack"`
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

func BuildHostState(cfg *config.Config, sbxSecretsOut string, sbxOK bool, dial func(int) bool, keysSource string, pack packinfo.State, githubGlobal bool) HostState {
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

	mcpServers := append([]string(nil), cfg.MCP...)

	hs := HostState{
		Keys:   keys,
		Memory: hostStateSvc{Enabled: slices.Contains(cfg.Services, "memory"), Up: dialer(rpc.MemoryPortDefault), Port: rpc.MemoryPortDefault},
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
	// GLOBAL only. `sbx secret ls` lists sandbox-scoped secrets too, and a
	// substring match on it reported github as available on a host where it was
	// pinned to a single sandbox: the agent believed that, committed, and could
	// not push. A credential one box can use is not one this payload may promise
	// to every box.
	ghState, _ := secret.ProbeGitHubSecret(env)
	hs := BuildHostState(cfg, sbxOut, sbxOK, env.DialLocal, source, packinfo.Resolve(cfg, packOverride),
		ghState == secret.GitHubSecretGlobal)
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
