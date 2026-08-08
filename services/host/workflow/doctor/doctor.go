// Package doctor is the two readiness surfaces: `pix status`, the glance, and
// `pix doctor`, the diagnosis. They are one package because they are one
// question asked at two depths, and keeping them apart is what let them
// disagree.
//
// Both build the SAME probe set (probes.go) and render the SAME Snapshot
// (health). What differs is deliberate and small:
//
//	status  short, no repair commands, ALWAYS exit 0. It is the landing
//	        screen and a scraper's `pix status || exit` must not fail because
//	        a probe could not see something from in here.
//	doctor  every probe, its evidence, and the exact commands. Exit 1 only
//	        for a REQUIRED probe that verified a gap; unknown alone is not a
//	        failure, because "I could not check" is not "you are broken".
//
// This replaces six group builders (providers, ollama, memory, gog, secrets,
// mcp), two renderers and two JSON schemas. The readiness model they were
// written against survives only where a leaf still needs it — the Google
// Workspace and MCP helpers below, which other workflows call.
package doctor

import (
	"context"
	"io"
	"sort"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/secret"
)

// RunDoctor probes the host and writes the full diagnosis, returning the
// process exit code.
//
// The exit contract is health's, not doctor's: 0 when nothing REQUIRED
// verified a gap, 1 when something did. An unknown never fails the process on
// its own. A host diagnosed from a bad vantage point (inside the sandbox,
// where sbx and launchctl do not exist) is not a broken host, and a doctor
// that exits 1 there teaches everyone to ignore its exit code.
func RunDoctor(ctx context.Context, cfg *config.Config, profile string, out io.Writer, o Options, jsonOut, verbose bool) int {
	if o.Budget == 0 {
		o.Budget = health.DoctorBudget
	}
	snap := Check(ctx, cfg, o)
	if jsonOut {
		_ = cli.WriteJSONOut(out, ReportJSON(snap, profile, snap.ExitCode()))
		return snap.ExitCode()
	}
	health.RenderDoctorWith(out, snap, health.DoctorOpts{Verbose: verbose})
	return snap.ExitCode()
}

// Description is the prose above doctor's GENERATED usage: what it proves and
// what its exit codes mean. The flag list is not here — the command struct's
// tags are the flag list, and a second copy could only disagree with it.
const Description = `Diagnose host health: the sbx CLI, the active pack, provider keys, the memory
unit, the monitor, and the LaunchAgent. Every check reports what it PROVED,
with the exact command that repairs a verified gap.

--json emits the machine-readable snapshot (schema_version 5).

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

// UnwiredProviderKeys is the gap this whole feature closes, as a fact both the
// status screen and doctor can read: a provider whose key RESOLVES on this host
// but which has no native binding in config, i.e. a key that is present,
// correct, and doing nothing.
//
// It reports absence of wiring, never a verdict about the key's validity. A
// binding that exists but failed its probe is NOT reported here — that is a
// different problem with a different fix, and conflating them would send a user
// to `models add` for a credential their provider rejected.
//
// Silent when a pack owns inference (its bindings are the pack's business) and
// when the key list is unreadable, since an unreadable list is not evidence of
// a gap.
func UnwiredProviderKeys(cfg *config.Config, env hostenv.Env) []string {
	if cfg == nil || cfg.Inference.ExclusiveSource != "" {
		return nil
	}
	names, err := secret.ProviderKeyNames(env)
	if err != nil || len(names) == 0 {
		return nil
	}
	bound := inference.BoundNativeProviders(cfg)
	var gaps []string
	for _, n := range names {
		if !bound[n] {
			gaps = append(gaps, n)
		}
	}
	sort.Strings(gaps)
	return gaps
}

// The sandbox-liveness tri/four-state (unknown/absent/running/stopped) that
// used to live here as SbxState moved to the L1 sandbox package (see
// sandbox.State in sandbox/list.go) — this package built the probe but never
// owned the vocabulary, and workflow/launch, the actual probe owner, now
// depends on the canonical type directly instead of doctor re-exporting it.
