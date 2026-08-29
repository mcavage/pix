// doctor_cmd.go — `pix doctor` and `pix status` as typed root children. They own an
// exit contract and nothing else: probing and rendering are workflow/doctor's, and a
// cli.SilentError carries doctor's own code, because its rendered table already said
// everything there is to say.
package main

import (
	"context"
	"os"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/health"
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
	cfg, profile, err := workspace.LoadResolvedConfig()
	if err != nil {
		// Doctor DOES fail here: an unreadable config is a verified gap in something
		// required. Status renders the same fact and exits 0.
		health.RenderDoctorWith(d.Out, doctor.ConfigLoadSnapshot(err), health.DoctorOpts{Verbose: c.Verbose})
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
	return nil
}

// statusCmd is the `status` verb AND the bare-`pix` landing screen: a fast,
// read-only glance answering "what state am I in, what is my next move", WITHOUT
// launching anything.
//
// It exits 0 whatever the verdict (doctor.StatusExit): the landing screen runs under
// `set -e` and in prompts, so a probe that could not see something must not take a
// shell down with it. The verdict is one `pix doctor` away.
func (c *statusCmd) Help() string { return doctor.StatusDescription }

type statusCmd struct {
	JSON bool `help:"Emit the machine-readable snapshot (schema_version 5)."`
}

func (c *statusCmd) Run(d *cli.Deps) error {
	cfg, profile, err := workspace.LoadResolvedConfig()
	if err != nil {
		// A config that will not load is an ISSUE on the glance, not a reason to take
		// the caller's shell down: status always exits 0, and the verdict rides in
		// --json's `exit` field.
		doctor.RenderStatusConfigError(d.Out, profile, err, c.JSON)
		return nil
	}
	doctor.RenderStatus(context.Background(), cfg, profile, d.Out, doctorOptions(), c.JSON)
	return nil
}

// doctorOptions fills the seams both surfaces share: the host environment and the
// workspace the MCP attachment answer is about. An unresolvable workspace stays
// empty, reported as "attachment unknown" rather than guessed.
func doctorOptions() doctor.Options {
	env := defaultShellEnv()
	// Same credentials registration uses, so a probe tests the command the
	// gateway will really spawn rather than whatever doctor's own shell holds.
	o := doctor.Options{Env: env, HostResolver: env.HostBinary, Credentials: mcpCredentials(env)}
	if ws, err := os.Getwd(); err == nil {
		o.Workspace = ws
	}
	return o
}
