// doctor_cmd.go — `pix doctor` and `pix status` as typed root children. Both
// own an exit contract and nothing else: the probing and the rendering are
// workflow/doctor's, and the process exit is the root's ONE mapper (a
// cli.SilentError carries doctor's own code, because doctor has already said
// everything there is to say in its rendered table).
package main

import (
	"context"
	"os"

	"pix/host/cli"
	"pix/host/health"
	"pix/host/workflow/doctor"
	"pix/host/workspace"
)

// doctorCmd is the diagnose-and-tell-me-the-fix verb. The exit contract is
// health's: 0 when nothing REQUIRED verified a gap (a check that could not be
// made from here does NOT fail the process), 1 when a required check verified
// one or the config failed to load, 2 for a usage error.
func (c *doctorCmd) Help() string { return doctor.Description }

type doctorCmd struct {
	JSON    bool `help:"Emit the machine-readable snapshot (schema_version 4)."`
	Verbose bool `help:"Show the evidence for every check, not only the ones that failed."`
}

func (c *doctorCmd) Run(d *cli.Deps) error {
	cfg, profile, err := workspace.LoadResolvedConfig()
	if err != nil {
		// Doctor DOES fail on this: an unreadable config is a verified gap in
		// something required, and doctor's whole contract is that exit 1 means
		// exactly that. Status renders the same fact and exits 0.
		health.RenderDoctorWith(d.Out, doctor.ConfigLoadSnapshot(err), health.DoctorOpts{Verbose: c.Verbose})
		return cli.SilentError{Code: health.ExitNotReady}
	}
	if code := doctor.RunDoctor(context.Background(), cfg, profile, d.Out, doctorOptions(), c.JSON, c.Verbose); code != health.ExitOK {
		return cli.SilentError{Code: code}
	}
	return nil
}

// statusCmd is the `status` verb AND the bare-`pix` landing screen: a fast,
// read-only glance answering "what state am I in, what's my next move" —
// WITHOUT launching anything. It replaces the old footgun where bare `pix`
// spun up a sandbox.
//
// It exits 0 whatever the verdict (doctor.StatusExit). The landing screen is
// what runs under `set -e` and in prompts; a probe that could not see
// something must not take a shell down with it, and the verdict is one
// `pix doctor` (or the `exit` field of --json) away.
func (c *statusCmd) Help() string { return doctor.StatusDescription }

type statusCmd struct {
	JSON bool `help:"Emit the machine-readable snapshot (schema_version 4)."`
}

func (c *statusCmd) Run(d *cli.Deps) error {
	cfg, profile, err := workspace.LoadResolvedConfig()
	if err != nil {
		// A config that will not load is an ISSUE on the glance, not a reason
		// to take the caller's shell down. Status always exits 0 (see
		// StatusExit); the verdict rides in --json's `exit` field.
		doctor.RenderStatusConfigError(d.Out, profile, err, c.JSON)
		return nil
	}
	doctor.RenderStatus(context.Background(), cfg, profile, d.Out, doctorOptions(), c.JSON)
	return nil
}

// doctorOptions fills the seams both surfaces share: the host environment and
// the workspace whose sandbox the MCP attachment answer is about. A workspace
// that cannot be resolved leaves Workspace empty, which is reported as
// "attachment unknown" rather than guessed.
func doctorOptions() doctor.Options {
	env := defaultShellEnv()
	o := doctor.Options{Env: env, HostResolver: env.HostBinary}
	if ws, err := os.Getwd(); err == nil {
		o.Workspace = ws
	}
	return o
}
