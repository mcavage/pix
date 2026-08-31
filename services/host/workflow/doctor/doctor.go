// Package doctor is `pix doctor`'s diagnosis: every probe, its evidence, and
// the exact repair command. `pix status` (and the bare-`pix` landing screen
// it once also served) is not part of the v2 CLI surface
// (docs/design/pix-v2-surface.md §3) — its dispatchable wrapper, this
// package's own short-form renderers for it, and the shared render helper
// underneath them were all unreachable dead code and are deleted (AC-16).
// UnreadableConfigSnapshot below is the one piece of that former file that
// survives: `pix doctor` itself renders it when the workspace config will
// not load.
//
// Everything below builds one probe set (probes.go) and renders one Snapshot
// (health). Exit 1 is reserved for a REQUIRED probe that verified a gap;
// unknown alone is not a failure, because "I could not check" is not "you are
// broken".
//
// This replaced six group builders (providers, ollama, memory, gog, secrets,
// mcp), two renderers and two JSON schemas. The readiness model they were
// written against survives only where a leaf still needs it — the Google
// Workspace and MCP helpers below, which other workflows call.
package doctor

import (
	"context"
	"fmt"
	"io"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/health"
)

// RunDoctor probes the host and writes the full diagnosis, returning the
// process exit code.
//
// The exit contract is health's, not doctor's: 0 when nothing REQUIRED
// verified a gap, 1 when something did. An unknown never fails the process on
// its own. A host diagnosed from a bad vantage point (inside the sandbox,
// where sbx and launchctl do not exist) is not a broken host, and a doctor
// that exits 1 there teaches everyone to ignore its exit code.
// UnreadableConfigSnapshot is what a config that would not load looks like as
// a health answer: one required check that VERIFIED a gap, with the parse
// error and the file it came from as evidence. Nothing else can be probed
// with no config, so it is the whole snapshot. There is no Fix command here
// on purpose: `pix config path` is not a real verb (v2 has no `config`
// command at all), so the only honest repair pointer is the path already
// named in Evidence.
func UnreadableConfigSnapshot(err error) health.Snapshot {
	return health.Snapshot{Results: []health.Result{{
		Name: "config", Status: health.StatusAbsent, Required: true,
		Detail:   "could not be loaded",
		Evidence: config.Path() + ": " + err.Error(),
	}}}
}

func RunDoctor(ctx context.Context, cfg *config.Config, profile string, out io.Writer, o Options, jsonOut, verbose bool) int {
	if o.Budget == 0 {
		o.Budget = health.DoctorBudget
	}
	snap := Check(ctx, cfg, o)
	// Native environments require sbx health.SbxMinVersion or later (PRD
	// docs/design/environments.md section 5.6, AC-20): a too-old or unparsable
	// version fails closed here even though it reads an ALREADY-VERIFIED gap
	// (a too-old version) — sbxProbeResult already flags that as a normal
	// StatusAbsent row — as well as one the base SbxProbe classification
	// deliberately still reports Unknown (unparsable output), which is why the
	// exit code is computed from the gate, not from snap.ExitCode() alone.
	exit := snap.ExitCode()
	var gateMsg string
	if sbxResult, ok := snap.Find("sbx"); ok {
		if blocked, found := health.SbxVersionGate(sbxResult); blocked {
			gateMsg = health.SbxVersionGateMessage(found)
			exit = health.ExitNotReady
		}
	}
	if jsonOut {
		// The gate still fails the exit code here; the exact prose line is a
		// human-readable-mode-only concern, same as every other rendered fix
		// text — the JSON schema already carries the sbx row's own
		// Fix/Detail/Evidence for a machine reader.
		_ = cli.WriteJSONOut(out, ReportJSON(snap, profile, exit))
		return exit
	}
	if gateMsg != "" {
		fmt.Fprint(out, gateMsg)
		fmt.Fprintln(out)
	}
	health.RenderDoctorWith(out, snap, health.DoctorOpts{Verbose: verbose})
	return exit
}

// Description is the prose above doctor's GENERATED usage: what it proves and
// what its exit codes mean. The flag list is not here — the command struct's
// tags are the flag list, and a second copy could only disagree with it.
const Description = `Diagnose host health, read-only: PIX_HOME and the release manifest, Docker
and the sbx CLI, provider keys, GitHub credentials, the pix-memory container,
and its sbx MCP registration. Every check reports what it PROVED, with the
exact command that repairs a verified gap. Doctor never repairs, registers,
restarts, authenticates, or rewrites configuration.

--json emits the machine-readable snapshot (schema_version 6).

exit codes:
  0  nothing required is verifiably broken — including checks that could not
     be made from here ("I could not check" is not "you are broken")
  1  a required check verified a gap
  2  usage error
`

// SbxInstallHint is shared by doctor, run, and setup because the nightly tap
// name is expected to change when MCP support stabilizes. README.md carries the
// one accepted non-Go copy and must change at the same time.
const SbxInstallHint = health.SbxInstallFix

// The sandbox-liveness tri/four-state (unknown/absent/running/stopped) that
// used to live here as SbxState moved to the L1 sandbox package (see
// sandbox.State in sandbox/list.go) — this package built the probe but never
// owned the vocabulary, and workflow/launch, the actual probe owner, now
// depends on the canonical type directly instead of doctor re-exporting it.
