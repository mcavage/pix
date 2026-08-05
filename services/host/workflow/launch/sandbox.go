// sandbox.go — sandbox liveness and the `ls`/`rm` verbs.
//
// The probe (running/stopped/absent/unknown) is what drives run's
// create-vs-reattach-vs-replace decision and task's teardown guard, so it lives
// with them rather than in either caller. `ls`/`rm` manage the pix-* sandboxes
// `run` and `task` create — scoped to pix-* names on purpose: this tool manages
// the sandboxes it made, not every sbx box on the host.
package launch

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/sandbox"
	"pix/host/workspace"
)

// SbxState is a package-local alias for the canonical sandbox.State, and
// SbxUnknown/Absent/Running/Stopped are launch's stable names for its four
// values. They are produced and consumed entirely within this package's probe.
type SbxState = sandbox.State

const (
	SbxUnknown = sandbox.StateUnknown // could not determine (sbx errored / no runner)
	SbxAbsent  = sandbox.StateAbsent  // sbx responded and the name is not present
	SbxRunning = sandbox.StateRunning // present, status column reads running
	SbxStopped = sandbox.StateStopped // present, any other status
)

// ProbeTaskSandbox classifies name from `sbx ls` into one of {running, stopped,
// absent, unknown}. A non-zero/errored sbx invocation (or a missing runner) is
// UNKNOWN, never absent, so a failed probe can never be read as "the sandbox
// was never created". BOUNDED: a hung sbx times out to UNKNOWN, so run/setup/
// task preflights degrade honestly instead of wedging.
func ProbeTaskSandbox(env hostenv.Env, name string) SbxState {
	out, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
		return SbxUnknown
	}
	return classifySbxListing(out, name)
}

// classifySbxListing is the row-reading half of the probe, shared with the
// teardown's tighter-bounded absent confirmation (reap.go's probeStateWithin)
// so the two can never disagree about what a listing row means.
func classifySbxListing(out, name string) SbxState {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && f[0] == name {
			if len(f) >= 3 && f[2] == "running" {
				return SbxRunning
			}
			return SbxStopped
		}
	}
	return SbxAbsent
}

// Ls lists the pix sandboxes on this host. It returns an error rather than
// exiting: the exit code is the root's one mapper, not this function's.
func Ls(env hostenv.Env, out io.Writer, jsonOut bool) error {
	if _, err := env.LookPath("sbx"); err != nil {
		return fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to list sandboxes")
	}
	// BOUNDED: a hung `sbx ls` fails with a message, never wedges.
	raw, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
		return fmt.Errorf("sbx ls failed: %v", err)
	}
	// DIR is sbx's own best-effort column, and it is labelled as sbx's: the
	// launcher used to overlay it with a workspace it had recorded at create
	// time, which was a second store of a fact only the runtime can answer for
	// a box that may since have been recreated by anyone.
	boxes := workspace.ParsePixBoxes(raw)
	if jsonOut {
		b, err := json.MarshalIndent(boxes, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(b))
		return nil
	}
	if len(boxes) == 0 {
		fmt.Fprintln(out, "No pix sandboxes. Start one with `pix run`.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tDIR")
	for _, b := range boxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", b.Name, b.State, b.Dir)
	}
	tw.Flush()
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Remove one:  pix rm <name>   (or `sbx rm -f <name>` for non-pix boxes)")
	return nil
}

// RmOptions is the already-parsed `pix rm` invocation. Parsing is the root
// parser's job; deciding what a removal MEANS is this package's.
type RmOptions struct {
	Names   []string
	All     bool
	Orphans bool
	// Force is `-f`/`--force`: the ONLY seam that force-removes, and it is
	// reachable only with EXPLICIT names (see Rm). An automatic sweep can never
	// set it — not by policy alone, but because Rm refuses the combination.
	Force bool
	// Except is the keep set for --all: a box is removed only when it is ABSENT
	// from it (`-k`/`--keep` is the short/long spelling the command layer maps
	// onto this same field).
	Except []string
	// Interactive is whether stdin/stdout is a terminal. A BARE `pix rm` (no
	// names, no --all/--orphans) is refused outright on a non-interactive one.
	Interactive bool
	// Teardown tunes the bounded, proof-gated removal path (see reap.go).
	Teardown TeardownOptions
}

// Rm removes pix sandboxes, in one of four explicitly-chosen shapes:
//
//   - `pix rm NAME...` — NON-FORCE `sbx rm <name>`, gated on the same
//     kernel-verified zero-reference proof the automatic reaper uses. A box
//     with a KEEP marker and zero references is removed without force: a keep
//     exists to stop the AUTOMATIC reaper, not to argue with the operator.
//   - `pix rm NAME --force` — the one forced seam, and only with explicit
//     names (AGENTS.md safety invariant #7).
//   - `pix rm --all [--keep NAME]` — every pix-* box ABSENT from the keep set,
//     each removed non-force. Never forced, even with --force (refused).
//   - `pix rm --orphans` — only sandboxes this host can prove it created
//     (a valid creation record whose instance id still matches the runtime's)
//     with zero live references and no keep. Never forced.
//
// A per-name failure is reported as it happens and summarised as exit 1 through
// a SilentError, because each cause was named.
func Rm(env hostenv.Env, out, errOut io.Writer, opts RmOptions) error {
	if err := validateRmShape(opts); err != nil {
		return err
	}
	if _, err := env.LookPath("sbx"); err != nil {
		return fmt.Errorf("sbx not found on PATH; install the Docker Sandboxes CLI to remove sandboxes")
	}
	if opts.Orphans {
		results, err := SweepOrphans(env, out, opts.Teardown)
		if err != nil {
			return err
		}
		for _, r := range results {
			if r.Verdict == TeardownFailed {
				return cli.SilentError{Code: 1}
			}
		}
		return nil
	}
	names := append([]string(nil), opts.Names...)
	if opts.All {
		// BOUNDED: the --all discovery listing is read-only, so a hung sbx
		// fails with a message rather than wedging (the `sbx rm -f` calls below
		// are mutating lifecycle commands and stay on env.Run).
		raw, timedOut, err := env.RunTimed("sbx", "ls")
		if timedOut || err != nil {
			return fmt.Errorf("sbx ls failed: %v", err)
		}
		keep := map[string]bool{}
		for _, k := range opts.Except {
			keep[k] = true
		}
		for _, b := range workspace.ParsePixBoxes(raw) {
			if !keep[b.Name] {
				names = append(names, b.Name)
			}
		}
		if len(names) == 0 {
			fmt.Fprintln(out, "No pix sandboxes to remove.")
			return nil
		}
	}

	failed := false
	for _, n := range names {
		if !strings.HasPrefix(n, "pix-") {
			fmt.Fprintf(errOut, "refusing %q: not a pix sandbox (use `sbx rm -f %s` for that)\n", n, n)
			failed = true
			continue
		}
		if opts.Force {
			if err := RemovePixSandbox(env, n); err != nil {
				fmt.Fprintf(errOut, "failed to remove %s: %v\n", n, err)
				failed = true
				continue
			}
			fmt.Fprintf(out, "removed %s (forced)\n", n)
			continue
		}
		// The session key IS the default sandbox name, so a default-named box's
		// lease state is found and cleared; a --name'd box has none and is simply
		// removed (bounded, non-force) with nothing to clear.
		res := TeardownSandbox(env, n, n, TriggerExplicit, opts.Teardown)
		switch {
		case res.Removed():
			fmt.Fprintf(out, "removed %s\n", n)
		case res.Verdict == TeardownKeptBusy:
			fmt.Fprintf(errOut, "refusing %q: %s — pass --force to remove it anyway\n", n, res.Detail)
			failed = true
		default:
			fmt.Fprintf(errOut, "failed to remove %s: %s\n", n, res.Detail)
			failed = true
		}
	}
	if failed {
		return cli.SilentError{Code: 1}
	}
	return nil
}

// validateRmShape refuses the invocations whose MEANING is undefined, before
// anything is listed or removed. Two of the three are safety refusals, not
// ergonomics:
//
//   - --force with --all/--orphans: force is reserved for an explicitly named
//     sandbox a human typed. A bulk forced sweep is exactly the shape AGENTS.md
//     safety invariant #7 and the reaper's no-force contract exist to prevent.
//   - a BARE `pix rm` on a non-interactive terminal: nothing named, nothing
//     confirmable, no terminal to ask. It refuses instead of silently
//     no-opping (or, worse, later silently acting).
func validateRmShape(opts RmOptions) error {
	if opts.All && opts.Orphans {
		return cli.Usagef("--all and --orphans mean different things (every pix box vs. only unreferenced pix-owned ones); pick one")
	}
	if opts.Force && (opts.All || opts.Orphans) {
		return cli.Usagef("--force removes an explicitly named sandbox only; --all/--orphans never force-remove")
	}
	if len(opts.Names) == 0 && !opts.All && !opts.Orphans {
		if !opts.Interactive {
			return cli.Usagef("refusing bare `pix rm` on a non-interactive terminal: name a sandbox, or pass --all/--orphans explicitly")
		}
		return cli.Usagef("name a sandbox to remove, or use --all/--orphans (see `pix rm --help`)")
	}
	return nil
}

// RemovePixSandbox force-removes name. It is reachable ONLY from an explicitly
// named `pix rm --force` (see Rm): the one forced seam, typed by a human.
func RemovePixSandbox(env hostenv.Env, name string) error {
	_, err := env.Run("sbx", "rm", "-f", name)
	return err
}

// LsDescription and RmDescription are the long help the root's generated usage
// prints. They live with the behaviour they describe, so a change to one shows
// up in the other's diff.
const LsDescription = `List the pix sandboxes on this host (name, state, ws dir). These are the
boxes 'pix run' and 'pix task' create. For every sbx sandbox (not just pix's),
use 'sbx ls'.`

const RmDescription = `Remove pix sandboxes. Scoped to pix-* names; use 'sbx rm' for other boxes.
Removal is NOT forced by default: it needs a kernel-verified proof that no
shell still references the sandbox. Only an explicitly named --force skips it.

  pix rm pix-tact                  remove one (non-force, zero-reference proof)
  pix rm pix-tact --force          force it (the only forced seam)
  pix rm pix-a pix-b               remove several
  pix rm --all --keep pix-pix      remove all but one (never forced)
  pix rm --orphans                 remove only unreferenced pix-owned boxes`
