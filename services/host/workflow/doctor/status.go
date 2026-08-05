package doctor

import (
	"context"
	"fmt"
	"io"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/health"
	"pix/host/launcher"
)

// ParseStatusArgs validates status flags: -h/--help returns cli.ErrHelpRequested,
// --json sets jsonOut, and any other token is a usage error (so a typo like
// --jsom fails loud instead of running silently as if no flag were given).
func ParseStatusArgs(argv []string) (jsonOut bool, err error) {
	for _, a := range argv {
		switch a {
		case "-h", "--help":
			return false, cli.ErrHelpRequested
		case "--json":
			jsonOut = true
		default:
			return false, fmt.Errorf("unknown flag %q", a)
		}
	}
	return jsonOut, nil
}

const StatusUsage = `usage: pix status [--json]

Fast, read-only glance: what is ready, what is missing, what could not be
checked from here. Launches nothing, changes nothing, and always exits 0.
Run ` + "`pix doctor`" + ` for the exact fix commands.

flags:
  --json   emit the machine-readable snapshot (schema_version 4)
`

// StatusExit is status's exit code, and it is a constant on purpose. Status is
// the landing screen and the thing scripts poll; failing it on a probe verdict
// makes `pix status` unusable in a shell that runs with `set -e`, and every
// caller who wants the verdict has `pix doctor` (or the `exit` field in
// --json, which still carries doctor's answer).
const StatusExit = health.ExitOK

// RenderStatus probes the host and writes the glance, returning StatusExit.
//
// It runs the SAME probes as doctor, under the shorter status budget: the
// landing screen may not tell a different story from the diagnosis, and the
// only honest way to guarantee that is to ask the same questions.
func RenderStatus(ctx context.Context, cfg *config.Config, profile string, out io.Writer, o Options, jsonOut bool) int {
	if o.Budget == 0 {
		o.Budget = health.StatusBudget
	}
	snap := Check(ctx, cfg, o)
	if jsonOut {
		// exit stays 0 (StatusExit) while the JSON reports what DOCTOR would
		// exit with, so a machine reader gets the verdict without a shell
		// having to treat "could not check" as a failure.
		_ = cli.WriteJSONOut(out, ReportJSON(snap, profile, snap.ExitCode()))
		return StatusExit
	}
	fmt.Fprintf(out, "pix %s\n", launcher.Version)
	health.RenderStatus(out, snap)
	return StatusExit
}
