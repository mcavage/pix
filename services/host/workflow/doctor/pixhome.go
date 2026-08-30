package doctor

import (
	"context"
	"time"

	"pix/host/container"
	"pix/host/health"
)

// pixhome.go is this unit's `pix doctor` v2 path (docs/design/
// pix-v2-surface.md §3.7): read-only probes over release identity, host
// prerequisites, the pix-memory container, and its sbx MCP registration.
// CheckHome never mutates anything — every probe it runs, like every probe
// in health, only ever inspects and reports.

// HomeDeps is every external read this unit's probe set needs, injected so
// a test drives CheckHome against fakes with no real Docker, sbx, or
// filesystem beyond a temp PIX_HOME.
type HomeDeps struct {
	// Home is the resolved PIX_HOME root (pixhome.Paths.Home).
	Home string

	Exec health.ExecChecker // docker/git availability

	ContainerRunner container.Runner
	ContainerSpec   container.Spec
	Prober          container.Prober

	MCPLister      health.MCPLister
	MCPServerName  string
	MCPExpectedURL string
}

// CheckHome runs every v2 probe concurrently under budget (zero uses
// health.DefaultBudget) and returns the Snapshot — the same model `pix
// doctor`'s v1 Check already renders, so a caller can fold this Snapshot's
// Results into the same report rather than inventing a second rendering.
func CheckHome(ctx context.Context, d HomeDeps, budget time.Duration) health.Snapshot {
	probes := []health.Probe{
		health.ReleaseInstalledProbe{Home: d.Home},
		health.DockerAvailableProbe{Exec: d.Exec},
		health.GitAvailableProbe{Exec: d.Exec},
		health.MemoryContainerProbe{Runner: d.ContainerRunner, Spec: d.ContainerSpec, Prober: d.Prober},
		health.MemoryMCPRegistrationProbe{Lister: d.MCPLister, ServerName: d.MCPServerName, ExpectedURL: d.MCPExpectedURL},
	}
	return health.Run(ctx, budget, probes...)
}
