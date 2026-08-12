package doctor

import (
	"context"
	"os"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/mcp"
	"pix/host/packinfo"
	"pix/host/rpc"
	"pix/host/secret"
	"pix/host/service"
)

// probes.go is where a config becomes a list of things to go and check. It is
// the whole of doctor's and status's knowledge of WHAT a healthy host is; both
// verbs render the same Snapshot, so they cannot disagree about the same host.
//
// The probes themselves live in health: they cross real boundaries (exec a
// binary, dial a port, stat a directory) and classify what they find. Nothing
// here interprets a result; that is the model's job.

// Options are the seams. The zero value is what the CLI uses; tests point the
// binaries at fixtures and the roots at temp dirs, so every probe still runs
// its real code path against a real process, socket or file.
type Options struct {
	// Budget bounds a single probe. Zero picks health.DefaultBudget.
	Budget time.Duration
	// PackOverride is `--pack`, which wins over the configured pack.
	PackOverride string
	// SbxBin, KeyStoreBin and LaunchctlBin name the executables. Empty means
	// the real ones, found on PATH.
	SbxBin       string
	KeyStoreBin  string
	LaunchctlBin string
	// SbxArgs, KeyStoreArgs and LaunchctlArgs override each probe's argv
	// (defaults: `--version`, `secret ls`, `print gui/<uid>/<label>`). They are
	// the seam the tests drive: a probe still execs a REAL process, it is just
	// pointed at a fixture that can be made to fail on purpose.
	SbxArgs       []string
	KeyStoreArgs  []string
	LaunchctlArgs []string
	// MemoryPort overrides the memory service port.
	MemoryPort int
	// LaunchdLabel and UID address the LaunchAgent. Zero UID means this
	// process's own.
	LaunchdLabel string
	UID          int
	// Credentials is the host's 1Password setup, used to wrap a server's
	// health probe in the SAME op-run command the gateway will spawn it with.
	// Zero means no 1Password, which leaves probes unwrapped.
	Credentials mcp.Credentials
	// Env and HostResolver are how the MCP probe learns what kind of server
	// each configured name is (the pack's declarations plus `pix-host mcp
	// --list`). A zero Env means the real host; a nil HostResolver means the
	// local inventory is UNKNOWN, which the classification fails closed on.
	Env hostenv.Env
	// GitHubScope overrides how the github row is answered. Nil means ask sbx.
	GitHubScope  func() (int, []string)
	HostResolver func() (string, error)
	// Workspace is the directory whose sandbox the attachment answer is
	// about. Empty means "no sandbox context", and attachment is unknown.
	Workspace string
	// MCPBin, MCPListArgs and MCPAuthArgs are the MCP probe's exec seams,
	// same contract as the others: a real process, pointed at a fixture.
	MCPBin      string
	MCPListArgs []string
	MCPAuthArgs []string
}

// Probes builds the host's probe set, in the order a report reads best:
// the CLI everything else needs, the pack that shapes the session, the keys
// that let a model answer, then the host service and the agent that keeps it
// alive.
//
// Every probe is included unconditionally. A capability the host has not
// enabled is reported as OPTIONAL, never omitted: a missing line is a fact a
// reader cannot see.
func Probes(cfg *config.Config, o Options) []health.Probe {
	sbxBin := orElse(o.SbxBin, "sbx")
	keyBin := orElse(o.KeyStoreBin, sbxBin)
	keyArgs := o.KeyStoreArgs
	if len(keyArgs) == 0 {
		keyArgs = []string{"secret", "ls"}
	}
	uid := o.UID
	if uid == 0 {
		uid = os.Getuid()
	}
	return []health.Probe{
		health.SbxProbe{Bin: sbxBin, Args: o.SbxArgs},
		health.PackProbe{Root: packinfo.ActivePackRoot(packOf(cfg), o.PackOverride)},
		// The model keys are ANY-OF: one of anthropic/openai/google is enough
		// to launch, which is the same definition `run`'s launch gate uses.
		// Reporting the other two as gaps would hand a working host two repair
		// commands it does not need.
		// Callable closes the gap between "the key store has a key" and "the router
		// can call that vendor": those are different facts, and only reporting the
		// first is how a host with three green providers routes every role to one.
		// Keyless is the prior question to both: whether a key is this host's
		// credential at all. A pack's sbx-session backends need none.
		health.ProviderKeyProbe{Bin: keyBin, Args: keyArgs, Want: secret.ModelProviders, AnyOf: true,
			Label: "providers", Callable: inference.CallableProviders(cfg), Keyless: inference.KeylessBackends(cfg)},
		health.MemoryUnitProbe{Port: portOr(o.MemoryPort, rpc.PortFromEnv("MEMORY_PORT", rpc.MemoryPortDefault)),
			Enabled: config.ServiceEnabled(cfg, "memory")},
		health.LaunchdProbe{Bin: o.LaunchctlBin, Label: orElse(o.LaunchdLabel, service.LaunchdLabel), UID: uid, Args: o.LaunchctlArgs},
		mcpProbe(cfg, o, sbxBin),
		health.DaemonProbe{Servers: DaemonServers(cfg)},
		// A sandbox holds no GitHub credential of its own, so without a global
		// secret the agent commits and then cannot push. Reported here rather
		// than discovered at the end of a task.
		health.GitHubSecretProbe{Fix: secret.GitHubSecretFix, Scope: githubScope(o)},
	}
}

// githubScope resolves the GitHub credential's scope, through the Options seam
// when a test supplies one. Tests cannot ask the real sbx: the answer would be
// whatever the developer's own machine happens to hold.
func githubScope(o Options) func() (int, []string) {
	if o.GitHubScope != nil {
		return o.GitHubScope
	}
	return func() (int, []string) {
		state, boxes := secret.ProbeGitHubSecret(o.Env)
		return int(state), boxes
	}
}

// DaemonServers resolves the active pack's supervised daemons into the shape
// health.DaemonProbe checks. Pure data from the manifest, like the MCP
// classification beside it — a pack that declares none yields none, and a pack
// that will not load yields none rather than a guess.
func DaemonServers(cfg *config.Config) []health.DaemonServer {
	if cfg == nil {
		return nil
	}
	var out []health.DaemonServer
	for _, root := range packinfo.ActivePackRoots(cfg, "") {
		p, err := packinfo.LoadPack(root)
		if err != nil {
			continue
		}
		for _, svc := range p.Manifest.Services {
			if svc.Runtime != packinfo.ServiceRuntimeDaemon || svc.Activation != "always" {
				continue
			}
			out = append(out, health.DaemonServer{
				Name: svc.Name, Listen: svc.Listen, Port: svc.Port, Health: svc.Health,
				Unpinned: svc.Command != "",
				// The supervisor owns the daemon's lifecycle, so the honest
				// repair is to restart the thing that supervises it — not to
				// go poking at the daemon directly.
				Fix: "pix serve stop && pix serve   # the supervisor restarts a pack daemon",
			})
		}
	}
	return out
}

// mcpProbe assembles the MCP probe from the one thing only this layer can
// resolve: the per-server classification, which decides which repair command
// is honest. An unclassifiable server degrades to "unknown", never to a guess.
func mcpProbe(cfg *config.Config, o Options, sbxBin string) health.MCPProbe {
	return health.MCPProbe{
		Servers:  MCPServers(cfg, o.Credentials),
		Bin:      orElse(o.MCPBin, sbxBin),
		ListArgs: o.MCPListArgs,
		AuthArgs: o.MCPAuthArgs,
	}
}

// Check runs the whole set and returns the one Snapshot both verbs render.
func Check(ctx context.Context, cfg *config.Config, o Options) health.Snapshot {
	return health.Run(ctx, o.Budget, Probes(cfg, o)...)
}

func packOf(cfg *config.Config) string {
	if cfg == nil {
		return ""
	}
	return cfg.Pack
}

func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func portOr(port, def int) int {
	if port > 0 {
		return port
	}
	return def
}
