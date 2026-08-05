package doctor

import (
	"context"
	"os"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/monitor"
	"pix/host/rpc"
	"pix/host/secret"
	"pix/host/service"
	"pix/host/workflow/pack"
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
	// MemoryPort and MonitorPort override the service ports.
	MemoryPort  int
	MonitorPort int
	// LaunchdLabel and UID address the LaunchAgent. Zero UID means this
	// process's own.
	LaunchdLabel string
	UID          int
	// Env and HostResolver are how the MCP probe learns what kind of server
	// each configured name is (the pack's declarations plus `pix-host mcp
	// --list`). A zero Env means the real host; a nil HostResolver means the
	// local inventory is UNKNOWN, which the classification fails closed on.
	Env          hostenv.Env
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
// that let a model answer, then the two host services and the agent that keeps
// them alive.
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
		health.PackProbe{Root: pack.ActivePackRoot(packOf(cfg), o.PackOverride)},
		// The model keys are ANY-OF: one of anthropic/openai/google is enough
		// to launch, which is the same definition `run`'s launch gate uses.
		// Reporting the other two as gaps would hand a working host two repair
		// commands it does not need.
		health.ProviderKeyProbe{Bin: keyBin, Args: keyArgs, Want: secret.ModelProviders, AnyOf: true, Label: "providers"},
		health.MemoryUnitProbe{Port: portOr(o.MemoryPort, rpc.PortFromEnv("MEMORY_PORT", rpc.MemoryPortDefault)),
			Enabled: config.ServiceEnabled(cfg, "memory")},
		health.MonitorProbe{Port: portOr(o.MonitorPort, monitor.DefaultPort),
			Enabled: config.ServiceEnabled(cfg, "monitor")},
		health.LaunchdProbe{Bin: o.LaunchctlBin, Label: orElse(o.LaunchdLabel, service.LaunchdLabel), UID: uid, Args: o.LaunchctlArgs},
		mcpProbe(cfg, o, sbxBin),
	}
}

// mcpProbe assembles the MCP probe from the one thing only this layer can
// resolve: the per-server classification, which decides which repair command
// is honest. An unclassifiable server degrades to "unknown", never to a guess.
func mcpProbe(cfg *config.Config, o Options, sbxBin string) health.MCPProbe {
	return health.MCPProbe{
		Servers:  MCPServers(cfg, o.Env, o.HostResolver),
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
