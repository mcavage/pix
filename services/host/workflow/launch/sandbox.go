// sandbox.go — sandbox liveness and the `ls`/`rm` verbs.
package launch

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"pix/host/cli"
	"pix/host/envinfo"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/lease"
	"pix/host/mcp"
	"pix/host/sandbox"
	"pix/host/workspace"
)

// SbxUnavailableErr is the ONE sbx-absent message every launcher surface that
// needs sbx to do its job (ls, rm, and task new's pre-checkout probe) returns
// — the SAME detectable sentinel (mcp.ErrSbxUnavailable, already the
// read/write mcp verbs' contract, mapped to rpc.ExitServiceDown by the
// command layer) and the SAME exact install fix doctor already prints
// (health.SbxInstallFix), instead of each caller paraphrasing "install the
// Docker Sandboxes CLI" differently with no exit code story of its own.
func SbxUnavailableErr(action string) error {
	return fmt.Errorf("sbx not on PATH; install it to %s: %s: %w", action, health.SbxInstallFix, mcp.ErrSbxUnavailable)
}

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
	return probeStateWithin(env, name, 0)
}

func probeStateWithin(env hostenv.Env, name string, within time.Duration) SbxState {
	var out string
	var timedOut bool
	var err error
	if within > 0 {
		out, timedOut, err = env.RunWithin(within, "sbx", "ls")
	} else {
		out, timedOut, err = env.RunTimed("sbx", "ls")
	}
	if timedOut || err != nil {
		return SbxUnknown
	}
	return classifySbxListing(out, name)
}

// classifySbxListing is the row-reading half of the probe.
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

// The HELD BY column's three answers. A sandbox with no live reference is one
// teardown WILL remove; "?" is reserved for state that could not be read, which
// must never render as free.
const (
	heldBySession = "session"
	heldByNone    = "—"
	heldUnknown   = "?"
)

// heldByColumn asks the lock, never a PID. lease/doc.go is explicit that a PID
// is reused the instant its owner exits and may not be treated as proof of
// liveness, so "who holds it" is answerable only as "someone does".
func heldByColumn(name string) string {
	dir, err := existingLeaseDir(name)
	if err != nil {
		// No lease state at all: nothing claims it. That is the orphan case,
		// and saying "—" is exactly right — `pix rm --orphans` sweeps it.
		if os.IsNotExist(err) {
			return heldByNone
		}
		return heldUnknown
	}
	held, err := lease.ReferencesHeld(dir)
	if err != nil {
		return heldUnknown
	}
	if held {
		return heldBySession
	}
	return heldByNone
}

func Ls(env hostenv.Env, out io.Writer, jsonOut bool) error {
	if _, err := env.LookPath("sbx"); err != nil {
		return SbxUnavailableErr("list sandboxes")
	}
	// BOUNDED: a hung `sbx ls` fails with a message, never wedges.
	raw, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
		return fmt.Errorf("sbx ls failed: %v", err)
	}
	// DIR is sbx's own best-effort column, and it is labelled as sbx's: never
	// overlaid with a workspace the launcher recorded at create time, a fact
	// only the runtime can answer for a box anyone may have recreated.
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
	fmt.Fprintln(tw, "NAME\tSTATE\tHELD BY\tDIR")
	anyHeld := false
	for _, b := range boxes {
		held := heldByColumn(b.Name)
		anyHeld = anyHeld || held == heldBySession
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.Name, b.State, held, b.Dir)
	}
	tw.Flush()
	fmt.Fprintln(out)
	if anyHeld {
		// The question this column exists to answer. A sandbox that outlives its
		// session looks like teardown is broken; usually it means a shell never
		// exited, and until now the only way to learn that was a journal nobody
		// reads and a lock nobody can see.
		// "session" and not "shell": the holder is a `pix` process ON THIS HOST,
		// and a shell EXEC'D INSIDE the sandbox holds no lease at all. The first
		// reader of this column asked which one it meant, which is the whole
		// answer to whether the word was right.
		fmt.Fprintln(out, "A held box has a live `pix` session on this host; it is removed when the last one exits.")
	}
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
func Rm(env hostenv.Env, out, errOut io.Writer, opts RmOptions) error {
	if err := validateRmShape(opts); err != nil {
		return err
	}
	if _, err := env.LookPath("sbx"); err != nil {
		return SbxUnavailableErr("remove sandboxes")
	}
	if opts.Orphans {
		results, err := sweepOrphans(env, out, opts.Teardown)
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
		// The final sandbox name is also its session key, so default and explicit
		// names both find and clear their own lease state. TriggerExplicit is
		// deliberate for both individually named arguments and `--all`: each is a
		// direct user removal request, so it may remove an unrecorded pix-* box and
		// ignores a keep marker. The bulk boundary is still strict: discovery is
		// filtered to pix-* names, --force cannot combine with --all, and a live
		// reference still returns kept-busy unless the user names one box with
		// explicit --force.
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

// RemovePixSandbox is the ONE forced seam: an explicitly-named `pix rm
// --force` (or a task-checkout removal already cleared by its own git-hygiene
// guard) that skips pix's own zero-holder-reference proof entirely — a human
// (or an equivalent already-proven guard) is vouching for this ONE name, not
// a wildcard. It routes through sandbox.PlanForceRemove for the exact same
// pix-* scope/name-safety check every other removal path uses, so this seam
// cannot reach a name PlanRemove would have refused either.
func RemovePixSandbox(env hostenv.Env, name string) error {
	argv, err := sandbox.PlanForceRemove(name)
	if err != nil {
		return err
	}
	_, err = env.Run("sbx", argv...)
	return err
}

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

// envRemoveFallbackReport is the exact explicit note docs/design/
// environments.md §10.3 requires when the stable effective file is absent:
// "It reports that environment-scoped secret cleanup could not run; it
// never guesses or prunes shared state." %q is the recorded pix-* instance
// name a caller is falling back to removing by name alone.
const envRemoveFallbackReport = "environment-scoped secret cleanup could not run: no effective environment file was found for %q; falling back to name-based removal (bindings and MCP registrations are host-global and are left untouched either way)"

// EnvRemovalPlan is what PlanEnvRemoveSeam composed: Argv is what a
// caller passes to `sbx` — never executed by this function, exactly like
// every other Plan* function in this module and in package sandbox — and
// Report is set ONLY on the fallback branch (see envRemoveFallbackReport).
type EnvRemovalPlan struct {
	Argv   []string
	Report string
}

// PlanEnvRemoveSeam is E2.4's launch-integration seam for docs/design/
// environments.md §10.3's environment-scoped removal: the future E2.5
// launch cutover's caller for "how do I tear down a sandbox this host may
// have launched through a stable effective environment file".
//
// It composes the SAME effectivePath a launch would have created (§6.2:
// "Create and remove always use this same path"), and when a file exists
// there, recomputes the effective name FROM THAT DOCUMENT — reading its own
// `name:` field back via envinfo.Parse, never re-deriving one independently
// — and plans through sandbox.PlanEnvRemove: refusing anything outside
// pix-* scope or unequal to recordedInstanceName before any argv is ever
// composed. A refusal here is a cli.UsageError (exit 2): the same posture
// validateRmShape already gives every other malformed-removal-intent
// refusal in this file, because a scope or instance mismatch is exactly
// that — an invocation this caller must not proceed with, not a
// transient failure.
//
// When effectivePath names no file — "as with a pre-migration sandbox or a
// hard crash that lost state" (§10.3) — PlanEnvRemoveSeam falls back
// to this package's EXISTING name-based planner, sandbox.PlanForceRemove
// when force is true or sandbox.PlanRemove otherwise: the IDENTICAL
// pix-*/name-safety scope check every other removal path in this file
// already runs. This function only PLANS that fallback argv; it never
// executes anything, so it can neither weaken nor skip the
// holder/keep/instance-id/fresh-probe proof chain TeardownSandbox already
// enforces before ever forwarding removal argv to sbx — a caller MUST
// still route the returned Argv through TeardownSandbox/RemovePixSandbox
// exactly as every other removal in this file does today. The fallback
// plan's Report states, in the operator's own words, that environment-
// scoped secret cleanup could not run, so a caller can surface that
// instead of silently pretending it happened.
//
// Neither branch ever appends `--prune-bindings`, or any flag beyond `-f`:
// see sandbox.PlanEnvRemove's own doc comment on A3's nonclaim.
func PlanEnvRemoveSeam(effectivePath, recordedInstanceName string, force bool) (EnvRemovalPlan, error) {
	if effectivePath != "" {
		_, statErr := os.Stat(effectivePath)
		switch {
		case statErr == nil:
			doc, perr := envinfo.Parse(effectivePath)
			if perr != nil {
				return EnvRemovalPlan{}, fmt.Errorf("launch: could not read effective environment file %s: %w", effectivePath, perr)
			}
			argv, rerr := sandbox.PlanEnvRemove(effectivePath, doc.Name, recordedInstanceName)
			if rerr != nil {
				return EnvRemovalPlan{}, cli.Usagef("%v", rerr)
			}
			return EnvRemovalPlan{Argv: argv}, nil
		case !os.IsNotExist(statErr):
			return EnvRemovalPlan{}, fmt.Errorf("launch: could not check effective environment file %s: %w", effectivePath, statErr)
		}
		// statErr is a plain "not exist": fall through to the name-based plan.
	}

	planFallback := sandbox.PlanRemove
	if force {
		planFallback = sandbox.PlanForceRemove
	}
	argv, rerr := planFallback(recordedInstanceName)
	if rerr != nil {
		return EnvRemovalPlan{}, cli.Usagef("%v", rerr)
	}
	return EnvRemovalPlan{
		Argv:   argv,
		Report: fmt.Sprintf(envRemoveFallbackReport, recordedInstanceName),
	}, nil
}
