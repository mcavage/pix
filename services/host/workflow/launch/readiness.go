// readiness.go is what `pix run` knows about host health: the ONE probe the
// launch gate turns on, and the handful of rows it is allowed to print before
// it gets out of the way.
//
// Two properties are load-bearing:
//
//  1. THE GATE IS TRI-STATE, AND ONLY A POSITIVE ANSWER REFUSES. `sbx secret
//     ls` either lists a model key, positively lists none, or could not be
//     asked. Only the middle case stops a launch (RefusesLaunch); an absent
//     sbx, a refused query or a timeout PROCEEDS, because a false refusal is
//     worse than a failed launch (safety invariant 6).
//  2. THE EVIDENCE IS PAID FOR ONCE. The gate's Result is handed back to
//     FastSnapshot rather than re-probed, so rendering readiness costs `run`
//     no second subprocess.
package launch

import (
	"context"
	"fmt"
	"io"

	"pix/host/config"
	"pix/host/health"
	"pix/host/rpc"
	"pix/host/secret"
)

// ProbeModelKeys runs the ONE launch-gate probe: does the key store list at
// least one model provider key. bin/args default to `sbx secret ls`; a test
// points them at a fixture executable so the real exec path still runs.
func ProbeModelKeys(ctx context.Context, bin string, args ...string) health.Result {
	if bin == "" {
		bin, args = "sbx", []string{"secret", "ls"}
	}
	snap := health.Run(ctx, health.StatusBudget, health.ProviderKeyProbe{
		Bin: bin, Args: args, Want: secret.ModelProviders, AnyOf: true, Label: "providers",
	})
	return snap.Results[0]
}

// RefusesLaunch reports whether the key probe POSITIVELY established that this
// host has no model key. Denied (the store refused the query) and unknown (it
// could not be asked) are not that answer and never stop a launch.
func RefusesLaunch(r health.Result) bool { return r.Effective() == health.StatusAbsent }

// FastSnapshot is the small snapshot the daily path renders: the key evidence
// `run` already paid for, plus the one host service whose absence silently
// degrades a session (recall). Everything else — ollama, models, pack, MCP —
// belongs to `pix doctor`, which is the command whose job is to be thorough.
//
// A zero-value keys Result (the gate did not run, e.g. no sbx on PATH or a
// keyless inference host) is omitted rather than rendered: absence of evidence
// is not a row.
func FastSnapshot(ctx context.Context, cfg *config.Config, keys health.Result) health.Snapshot {
	snap := health.Run(ctx, health.StatusBudget, health.MemoryUnitProbe{
		Port:    rpc.PortFromEnv("MEMORY_PORT", rpc.MemoryPortDefault),
		Enabled: config.ServiceEnabled(cfg, "memory"),
	})
	if keys.Name != "" {
		snap.Results = append([]health.Result{keys}, snap.Results...)
	}
	return snap
}

// WarningLimit is how many readiness rows `pix run` may print before it stops
// and points at doctor. A wall of readiness text on the daily command trains
// the user to ignore all of it, so the limit is small and the remainder is a
// count, not more rows.
const WarningLimit = 3

// RenderWarnings prints at most limit non-ready rows through health's glyph
// vocabulary (a fast surface never spells a glyph itself), then a single
// "N more" pointer at doctor. It returns the TOTAL number of warnings, not the
// number printed, so a caller can report the true tally. It never blocks and
// never exits: the only thing that stops a launch is the key gate above.
//
// Verified gaps sort before unknowns — "this is broken" earns the scarce rows
// ahead of "this could not be checked from here".
func RenderWarnings(w io.Writer, s health.Snapshot, limit int) int {
	rows := append(s.Gaps(), s.Unknown()...)
	if len(rows) == 0 {
		return 0
	}
	shown := rows
	if limit > 0 && len(rows) > limit {
		shown = rows[:limit]
	}
	for _, r := range shown {
		fmt.Fprintf(w, "  %s %s: %s\n", health.Glyph(r.Effective()), r.Name, r.Detail)
		// health.Run has already stripped the fix from anything that is not a
		// verified gap, so a row that carries one has earned it.
		if r.Fix != "" {
			fmt.Fprintf(w, "      fix: %s\n", r.Fix)
		}
	}
	if n := len(rows) - len(shown); n > 0 {
		fmt.Fprintf(w, "  (%d more: run `%s`)\n", n, health.DoctorCommand)
	}
	return len(rows)
}
