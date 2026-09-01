// doctor_cmd.go — `pix doctor` as a typed root child. It owns an
// exit contract and nothing else: probing and rendering are workflow/doctor's, and a
// cli.SilentError carries doctor's own code, because its rendered table already said
// everything there is to say.
package main

import (
	"context"
	"os"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/container"
	"pix/host/health"
	"pix/host/pixhome"
	"pix/host/stack"
	"pix/host/workflow/doctor"
	"pix/host/workspace"
)

// doctorCmd is the diagnose-and-tell-me-the-fix verb. The exit contract is health's:
// 0 when nothing REQUIRED verified a gap (a check that could not be MADE does not
// fail the process), 1 when one did or the config failed to load, 2 for usage.
func (c *doctorCmd) Help() string { return doctor.Description }

type doctorCmd struct {
	JSON      bool `help:"Emit the machine-readable snapshot (schema_version 5)."`
	Verbose   bool `help:"Show the evidence for every check, not only the ones that failed."`
	Recreates bool `help:"Show every recorded environment recreate: timestamp, environment, and the drifted canonical key paths (docs/design/environments.md section 9.4)."`
}

func (c *doctorCmd) Run(d *cli.Deps) error {
	// --recreates is its own diagnostic view, not an eighth `env` verb
	// (docs/design/environments.md §9.4) and not gated on a loadable config:
	// it reads the I4 log directly and never runs a probe.
	if c.Recreates {
		dir, derr := config.StateDir()
		if derr != nil {
			return derr
		}
		return doctor.RenderRecreates(d.Out, dir)
	}
	// The v2 PIX_HOME probe set (release identity, Docker/Git, the pix-memory
	// container, its reserved MCP registration) runs first and read-only,
	// independent of whether the (still-live, v1) workspace config loads at
	// all — CheckHome never mutates anything, so a config load failure below
	// must not suppress it. A required gap here fails the process even when
	// every v1 check below still passes.
	homeFailed := false
	if home, herr := pixhome.Resolve(); herr == nil {
		spec := homeContainerSpec(home)
		// config.toml's own DefaultEnvironment, read independently of the
		// (v1) workspace profile config below — EnvironmentDefaultProbe must
		// never disagree with what `pix env`/`pix run` themselves resolve.
		var defaultEnv string
		if hc, cerr := config.LoadFrom(config.PathAt(home.Home)); cerr == nil {
			defaultEnv = hc.DefaultEnvironment
		}
		// MCPServerName is THIS PIX_HOME's own scoped pix-memory MCP name
		// (Wave B coexistence) — wired even though MCPLister stays unset
		// below, so the probe's own name is never wrong the day a lister
		// adapter is added.
		var mcpServerName string
		if id, ierr := stack.ID(home.Home); ierr == nil {
			mcpServerName, _ = stack.MCPMemoryName(id)
		}
		ctx, cancel := context.WithTimeout(context.Background(), health.DefaultBudget)
		snap := doctor.CheckHome(ctx, doctor.HomeDeps{
			Home:               home.Home,
			Exec:               execChecker{},
			ContainerRunner:    container.DefaultRunner,
			ContainerSpec:      spec,
			Prober:             container.HTTPProber{},
			DefaultEnvironment: defaultEnv,
			MCPServerName:      mcpServerName,
			// MCPLister is deliberately unset: sbx has no machine-readable MCP
			// listing (homeadapters.go's own doc comment), so this probe
			// reports StatusUnknown rather than guessing a URL match.
		}, health.DefaultBudget)
		cancel()
		if !c.JSON {
			health.RenderDoctorWith(d.Out, snap, health.DoctorOpts{Verbose: c.Verbose})
		}
		homeFailed = snap.ExitCode() != health.ExitOK
	}
	cfg, profile, err := workspace.LoadResolvedConfig()
	if err != nil {
		// Doctor DOES fail here: an unreadable config is a verified gap in something
		// required. Status renders the same fact and exits 0.
		health.RenderDoctorWith(d.Out, doctor.UnreadableConfigSnapshot(err), health.DoctorOpts{Verbose: c.Verbose})
		return cli.SilentError{Code: health.ExitNotReady}
	}
	code := doctor.RunDoctor(context.Background(), cfg, profile, d.Out, doctorOptions(), c.JSON, c.Verbose)
	if !c.JSON {
		// Best-effort: the recreate-log pointer line is diagnostic-only, and a
		// missing/unreadable state dir must never change doctor's own exit code.
		if dir, derr := config.StateDir(); derr == nil {
			_ = doctor.RecreateSummaryLine(d.Out, dir)
		}
	}
	if code != health.ExitOK {
		return cli.SilentError{Code: code}
	}
	if homeFailed {
		return cli.SilentError{Code: health.ExitNotReady}
	}
	return nil
}

// The `status` verb (and the bare-`pix` landing screen it once also served)
// is not part of the v2 CLI surface (docs/design/pix-v2-surface.md §3;
// root.go's own doc comment names it among the removed verbs) — its
// dispatchable wrapper (statusCmd) and workflow/doctor's own short-form
// renderers for it were all unreachable dead code and are deleted (AC-16).
// doctor.UnreadableConfigSnapshot is the one piece that survives, above,
// because `pix doctor` itself renders it.

// doctorOptions fills the seams both surfaces share: the host environment and the
// workspace the MCP attachment answer is about. An unresolvable workspace stays
// empty, reported as "attachment unknown" rather than guessed.
func doctorOptions() doctor.Options {
	env := defaultShellEnv()
	// Same credentials registration uses, so a probe tests the command the
	// gateway will really spawn rather than whatever doctor's own shell holds.
	o := doctor.Options{Env: env}
	if ws, err := os.Getwd(); err == nil {
		o.Workspace = ws
	}
	return o
}
