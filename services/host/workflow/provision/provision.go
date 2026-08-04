// Package provision is the one setup loop: CHECK, apply what is missing,
// CHECK AGAIN — and success comes only from the second check.
//
// It replaces the setup/onboard phase machinery (ordered mutation steps, a
// receipt writer, a prompt budget, a handoff, and their several private
// notions of "done") with the shape all of that was approximating. The rules
// are small and each one is a real incident:
//
//   - The second check is authoritative. An apply that returns nil has
//     reported success; it has not PROVEN it. Provisioning that trusts the
//     mutation instead of re-probing is how "setup complete" ships next to a
//     host that cannot launch.
//   - Only a VERIFIED gap is applied. Unknown means the probe could not see;
//     mutating on that is guessing with the user's machine. Denied means the
//     org said no; applying cannot help.
//   - A step that is already ready is never touched. Provisioning is
//     idempotent because it re-derives what to do from the first check, not
//     from a receipt of what happened last time.
package provision

import (
	"context"
	"fmt"
	"io"
	"time"

	"pix/host/health"
)

// Step is one capability provisioning can both check and repair. Apply may be
// nil for a capability nothing here can fix (the report then names the gap and
// the exact manual command from the probe's own Fix).
type Step struct {
	Name  string
	Probe health.Probe
	Apply func(ctx context.Context) error
}

// Options tunes the loop. The zero value is the normal one.
type Options struct {
	// Budget bounds a single probe in both checks.
	Budget time.Duration
}

// Skip is a gap provisioning deliberately did not touch, and why.
type Skip struct {
	Name   string
	Reason string
}

// Failure is an apply that returned an error.
type Failure struct {
	Name string
	Err  error
}

// Unverified is the important one: the apply reported success and the second
// check disagreed.
type Unverified struct {
	Name   string
	Reason string
}

// Outcome is the whole record of one provisioning run: both checks, what was
// applied, and every way a step failed to end up proven.
type Outcome struct {
	Before     health.Snapshot
	After      health.Snapshot
	Applied    []string
	Skipped    []Skip
	Failed     []Failure
	Unverified []Unverified
}

// Run performs the loop. It never returns an error: every failure mode is a
// field on the Outcome, because a provisioning run that half-worked is the
// normal case and the caller needs the detail, not an error string.
func Run(ctx context.Context, opts Options, steps ...Step) Outcome {
	probes := make([]health.Probe, 0, len(steps))
	for _, s := range steps {
		probes = append(probes, named{name: stepName(s), Probe: s.Probe})
	}
	o := Outcome{Before: health.Run(ctx, opts.Budget, probes...)}

	for _, s := range steps {
		before, ok := o.Before.Find(stepName(s))
		if !ok {
			continue
		}
		switch {
		case before.OK():
			// Already proven. Touching it can only make things worse.
			continue
		case before.Effective() == health.StatusUnknown:
			o.Skipped = append(o.Skipped, Skip{stepName(s), "unknown: the probe could not verify this, so nothing was changed"})
			continue
		case before.Effective() == health.StatusDenied:
			o.Skipped = append(o.Skipped, Skip{stepName(s), "denied by policy: no setup step can fix this"})
			continue
		case s.Apply == nil:
			o.Skipped = append(o.Skipped, Skip{stepName(s), "no automatic fix: run the command in the report"})
			continue
		case ctx.Err() != nil:
			o.Skipped = append(o.Skipped, Skip{stepName(s), "cancelled before it was applied"})
			continue
		}
		if err := s.Apply(ctx); err != nil {
			o.Failed = append(o.Failed, Failure{stepName(s), err})
			continue
		}
		o.Applied = append(o.Applied, stepName(s))
	}

	// The second check. This is the only thing that can call anything ready.
	o.After = health.Run(ctx, opts.Budget, probes...)
	for _, s := range steps {
		after, ok := o.After.Find(stepName(s))
		if !ok || after.OK() {
			continue
		}
		if contains(o.Applied, stepName(s)) {
			o.Unverified = append(o.Unverified, Unverified{stepName(s),
				"apply reported success but the second check still finds it " + string(after.Effective())})
		}
	}
	return o
}

// named gives a step's results the STEP's identity rather than the probe's.
// Two steps may legitimately reuse the same probe type (two packs, two
// services); keying the outcome off the probe's own name silently merges them,
// which reads as "the second one is already fine".
type named struct {
	name string
	health.Probe
}

func (n named) Name() string { return n.name }

func (n named) Check(ctx context.Context) health.Result {
	r := n.Probe.Check(ctx)
	r.Name = n.name
	return r
}

// stepName falls back to the probe's own name when a Step does not name itself.
func stepName(s Step) string {
	if s.Name != "" {
		return s.Name
	}
	return s.Probe.Name()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Verified reports whether the SECOND check proved this step ready. It is the
// only success predicate in this package.
func (o Outcome) Verified(name string) bool {
	r, ok := o.After.Find(name)
	return ok && r.OK()
}

// Ready reports whether the second check proved every required capability.
func (o Outcome) Ready() bool { return o.After.Ready() }

// ExitCode is the second check's exit code: a verified gap in something
// required fails, an unknown does not.
func (o Outcome) ExitCode() int { return o.After.ExitCode() }

// Render writes the human report: what was applied, what was skipped and why,
// what failed, and the exact commands still outstanding.
func (o Outcome) Render(w io.Writer) {
	if len(o.Applied) > 0 {
		for _, n := range o.Applied {
			status := "verified"
			if !o.Verified(n) {
				status = "NOT verified by the second check"
			}
			fmt.Fprintf(w, "  applied  %-12s %s\n", n, status)
		}
	}
	for _, s := range o.Skipped {
		fmt.Fprintf(w, "  skipped  %-12s %s\n", s.Name, s.Reason)
	}
	for _, f := range o.Failed {
		fmt.Fprintf(w, "  failed   %-12s %v\n", f.Name, f.Err)
	}
	fmt.Fprintln(w)
	health.RenderDoctor(w, o.After)
}
