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

// StatusDescription is the prose above status's GENERATED usage.
const StatusDescription = `Fast, read-only glance: what is ready, what is missing, what could not be
checked from here. Launches nothing, changes nothing, and always exits 0.
Run ` + "`pix doctor`" + ` for the exact fix commands.

--json emits the machine-readable snapshot (schema_version 6).
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

// ConfigLoadSnapshot is what a config that would not load looks like as a
// health answer: one required check that VERIFIED a gap, with the exact
// command that shows the reader where the file is.
//
// It exists so a broken config is RENDERED by the same surfaces as everything
// else instead of being a bare stderr line and an exit code. Nothing else can
// be probed with no config, so it is the whole snapshot.
func ConfigLoadSnapshot(err error) health.Snapshot {
	return health.Snapshot{Results: []health.Result{{
		Name: "config", Status: health.StatusAbsent, Required: true,
		Detail:   "could not be loaded",
		Fix:      "pix config path",
		Evidence: config.Path() + ": " + err.Error(),
	}}}
}

// RenderStatusConfigError renders a config-load failure as an ISSUE on the
// glance and returns StatusExit — zero, like every other status outcome.
//
// This is the whole point of status's exit contract: the landing screen runs
// in prompts and under `set -e`, and a user whose config is broken needs to
// SEE that, not have their shell die on it. The verdict is still published —
// the JSON `exit` field carries doctor's 1 — so no machine reader loses
// anything.
func RenderStatusConfigError(out io.Writer, profile string, err error, jsonOut bool) int {
	snap := ConfigLoadSnapshot(err)
	if jsonOut {
		_ = cli.WriteJSONOut(out, ReportJSON(snap, profile, snap.ExitCode()))
		return StatusExit
	}
	fmt.Fprintf(out, "pix %s\n", launcher.Version)
	health.RenderStatus(out, snap)
	return StatusExit
}
