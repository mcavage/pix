// readiness.go is what `pix run` knows about host health: the ONE probe the
// launch gate turns on, and the handful of rows it may print before it gets out
// of the way. Two properties are load-bearing: the gate is TRI-STATE and only a
// POSITIVE "no model key" answer refuses (an absent sbx, a refused query or a
// timeout PROCEEDS — a false refusal is worse than a failed launch, safety
// invariant 6), and the evidence is paid for ONCE (the gate's Result is handed
// to FastSnapshot rather than re-probed).
package launch

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/secret"
)

// ProbeModelKeys runs the ONE launch-gate probe: does THIS PIX_HOME configure
// at least one model provider op:// ref. It reads the refs file and nothing
// else — no `sbx secret ls`, because a host-wide sbx secret is not this
// launcher's credential (see bootstrap.go) and answering the gate with one is
// how a stack with no keys of its own launched on another stack's.
//
// TRI-STATE, unchanged in meaning: a refs file that exists and could not be
// read is StatusUnknown and PROCEEDS; only a positively answered "no model ref
// is configured" is StatusAbsent, and only that refuses.
func ProbeModelKeys(env hostenv.Env) health.Result {
	names, state := secret.ConfiguredModelRefs(env)
	if state != secret.RefsAnswered {
		return health.Result{Name: "providers", Status: health.StatusUnknown, Required: true,
			Detail:   "could not read the configured refs",
			Evidence: "reading " + secret.DefaultOpRefsPath() + " failed"}
	}
	if len(names) == 0 {
		return health.Result{Name: "providers", Status: health.StatusAbsent, Required: true,
			Detail:   "no model provider ref is configured",
			Fix:      fmt.Sprintf(health.SecretSetFix, "ANTHROPIC_API_KEY"),
			Evidence: secret.DefaultOpRefsPath() + " configures none of " + strings.Join(secret.ModelProviders, ", ")}
	}
	return health.Result{Name: "providers", Status: health.StatusReady, Required: true,
		Detail:   strings.Join(names, ", ") + " configured",
		Evidence: "op:// refs in " + secret.DefaultOpRefsPath() + ", resolved into each sandbox at launch"}
}

// RefusesLaunch reports whether the key probe POSITIVELY established that this
// host has no model key. Denied (the store refused the query) and unknown (it
// could not be asked) are not that answer and never stop a launch.
func RefusesLaunch(r health.Result) bool { return r.Effective() == health.StatusAbsent }

// memoryLivenessProbe is a plain TCP dial to the pix-memory container's
// published loopback port — evidence that SOMETHING is listening, not a
// version- or identity-verified answer. `pix doctor`'s
// health.MemoryContainerProbe is the thorough, Docker-state-verified check;
// this one only needs to be fast and cheap enough to run on every `pix run`.
// The custom memory JSON-RPC identity call this used to make was deleted in
// the Pix v2 cutover along with the daemon that answered it (AC-16).
type memoryLivenessProbe struct {
	Port    int
	Enabled bool
}

func (memoryLivenessProbe) Name() string     { return "memory" }
func (p memoryLivenessProbe) Required() bool { return false }

func (p memoryLivenessProbe) Check(ctx context.Context) health.Result {
	if !p.Enabled {
		return health.Result{Name: p.Name(), Status: health.StatusOff, Detail: "not enabled"}
	}
	d := net.Dialer{Timeout: 500 * time.Millisecond}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", p.Port))
	if err != nil {
		return health.Result{Name: p.Name(), Status: health.StatusAbsent, Required: false,
			Detail: fmt.Sprintf("not reachable on :%d", p.Port), Fix: "pix doctor",
			Evidence: fmt.Sprintf("dialing 127.0.0.1:%d: %v", p.Port, err)}
	}
	_ = conn.Close()
	return health.Result{Name: p.Name(), Status: health.StatusReady, Detail: fmt.Sprintf("listening on :%d", p.Port)}
}

// FastSnapshot is the small snapshot the daily path renders: the key evidence
// `run` already paid for, plus the one host service whose absence silently
// degrades a session (recall). Everything else belongs to `pix doctor`, whose
// job is to be thorough. A zero-value keys Result (the gate did not run) is
// omitted rather than rendered: absence of evidence is not a row.
func FastSnapshot(ctx context.Context, cfg *config.Config, keys health.Result) health.Snapshot {
	snap := health.Run(ctx, health.StatusBudget, memoryLivenessProbe{
		Port:    memoryPortDefault,
		Enabled: true, // pix-memory is a reserved built-in in v2, never an opt-in service list entry
	})
	if keys.Name != "" {
		snap.Results = append([]health.Result{keys}, snap.Results...)
	}
	return snap
}

// WarningLimit is how many readiness rows `pix run` may print before it stops
// and points at doctor. A wall of readiness text on the daily command trains
// the user to ignore all of it.
const WarningLimit = 3

// RenderWarnings prints at most limit non-ready rows through health's glyph
// vocabulary (a fast surface never spells a glyph itself), then a single
// "N more" pointer at doctor. It returns the TOTAL number of warnings, not the
// number printed. It never blocks and never exits: the only thing that stops a
// launch is the key gate above. Verified gaps sort before unknowns — "this is
// broken" earns the scarce rows ahead of "this could not be checked".
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
