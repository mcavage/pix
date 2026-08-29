//go:build unix

// reap.go — the LAST-SHELL TEARDOWN and the orphan reaper: the half of the
// lifecycle session.go leaves out. The safety argument, in enforcement order:
//
//	this shell's refs SH is CLOSED first  ->  lease.TryReapProof (lifecycle EX +
//	refs EX, both NON-BLOCKING: zero live references, no transition in flight)
//	->  under that proof: a valid creation record, fingerprint and invocation;
//	the SAME immutable instance id re-probed from the runtime; no valid
//	identity-bound keep; a pix-* name this domain owns  ->  a bounded
//	`sbx rm -f <name>`  ->  a POSITIVE absent probe  ->  only then is the
//	lease/keep/fingerprint/invocation state cleared.
//
// Every weaker answer — a held reference, a held keep, an untrusted probe, an
// instance id that no longer matches the record, a name outside pix-* — KEEPS
// the sandbox and journals why.
//
// WHY `-f` APPEARS IN THIS FILE AT ALL (sbx v0.38): a bare `sbx rm` now
// prompts for confirmation and refuses outright with no TTY attached, and
// every call here runs from a non-interactive pix-host subprocess. `-f` is
// therefore composed via sandbox.PlanForceRemove purely to skip a prompt
// nobody here could ever answer — it is a TRANSPORT bypass of sbx's
// confirmation, not an authority bypass of pix's own gate. The authority
// gate is unchanged and is exactly what it always was: lease.TryExclusive()'s
// kernel-verified zero-holder proof (via lease.TryReapProof) for the
// automatic teardown and the orphan sweep, or — when there is no lease state
// left to prove a reference against at all — an explicitly, individually
// named removal intent with nothing else to check. Neither ever widens to a
// wildcard or to a name outside pix-*. The ONE seam that skips the
// zero-holder proof itself (not merely sbx's confirmation prompt) remains an
// explicitly-named `pix rm --force` typed by a human (sandbox.go's
// RemovePixSandbox, routed through the SAME sandbox.PlanForceRemove).
//
// A proven teardown clears the session's state COMPLETELY, directory included.
// That only works while this domain owns every file in the lease directory:
// lease.ClearState deliberately LEAVES a directory holding a file it does not
// own, so a second store co-located here makes ENOTEMPTY the ordinary outcome
// and leaks a directory per reaped session. assertLeaseStateCleared keeps it
// closed; anything tempted to co-locate state here again owns that regression.
package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/lease"
	"pix/host/sandbox"
	"pix/host/uat"
)

// The teardown budget, as three bounds that compose to one ceiling:
const (
	TeardownRmTimeout    = 15 * time.Second
	TeardownProbeTimeout = 3 * time.Second
	TeardownProbeRetries = 2
	TeardownBudget       = 20 * time.Second
	teardownProbeDelay   = 250 * time.Millisecond
)

// TeardownVerdict is what a teardown attempt DID, as a stable string so it can
// be journalled, tested and printed without a second mapping table.
type TeardownVerdict string

const (
	// TeardownRemoved: the sandbox was removed (non-force), its absence was
	// positively confirmed, and its lease state was cleared.
	TeardownRemoved TeardownVerdict = "removed"
	// TeardownAlreadyAbsent: the recorded sandbox is positively gone (nobody
	// removed anything here); only its stale lease state was cleared, which is
	// what keeps the NEXT create for this key from being refused as a relabel.
	TeardownAlreadyAbsent TeardownVerdict = "already-absent"
	// TeardownKeptBusy: another shell holds a live reference, or a transition is
	// in flight. The proof is non-blocking by design.
	TeardownKeptBusy TeardownVerdict = "kept-busy"
	// TeardownKeptKeep: an identity-bound keep is held (`pix run -k`).
	TeardownKeptKeep    TeardownVerdict = "kept-keep"
	TeardownKeptUnowned TeardownVerdict = "kept-unowned"
	// TeardownKeptMismatch: the runtime's instance id is not the recorded one —
	// the name was reused for a DIFFERENT instance.
	TeardownKeptMismatch TeardownVerdict = "kept-mismatch"
	// TeardownKeptUnknown: a probe could not be trusted, the budget ran out, or
	// state was unreadable. Fail closed: keep it.
	TeardownKeptUnknown TeardownVerdict = "kept-unknown"
	TeardownFailed      TeardownVerdict = "failed"
)

// TeardownTrigger is what asked for the teardown, and it decides the two
// permissions that differ between the automatic and the explicit paths.
type TeardownTrigger string

const (
	TriggerSession     TeardownTrigger = "session"
	TriggerOrphanSweep TeardownTrigger = "orphan-sweep"
	TriggerExplicit    TeardownTrigger = "explicit"
)

// requiresOwnership / honorsKeep are the trigger's two permissions, as methods
// so one place answers "what may this trigger do".
func (t TeardownTrigger) requiresOwnership() bool { return t != TriggerExplicit }
func (t TeardownTrigger) honorsKeep() bool        { return t != TriggerExplicit }

type TeardownOptions struct {
	RmTimeout    time.Duration
	ProbeTimeout time.Duration
	ProbeRetries int
	Budget       time.Duration
	ProbeDelay   time.Duration
	// JournalPath is the teardown journal; empty resolves TeardownJournalPath.
	JournalPath string
	// Now is the clock the budget is measured against, injectable so a test
	// asserts the bound instead of sleeping through it.
	Now func() time.Time
	// Planner composes the removal ARGV that gets EXECUTED — but only from
	// the two call sites in this file that have already cleared this
	// domain's own authority gate: an explicitly-named removal intent with
	// no lease state left to prove a reference against, or (inside
	// teardownUnderProof) every zero-holder/keep/instance-id/fresh-probe
	// proof this file enforces, in that order. Planning itself is inert —
	// it returns argv and stops, exactly like every sandbox.Plan* function —
	// so it is safe to call at either of those two already-authorized
	// points; what makes E2.5's environment-scoped argv safe to EXECUTE is
	// that removeAndConfirm, the one place Planner's output ever reaches
	// env.RunWithin, is reachable only from those same two points. A caller
	// cannot use Planner to skip a proof: the pix-* scope guard at the top
	// of decideTeardown is a FIXED check, not this field, and runs before
	// either point regardless of what Planner is set to.
	//
	// Nil (the default, set by withDefaults) is byte-compatible with the
	// pre-E2.5 hardcoded behavior: sandbox.PlanForceRemove(name), no Report.
	Planner TeardownPlanner
}

// TeardownPlanner composes the argv a teardown will execute for name, and an
// optional human-readable Report surfaced in the result's Detail — the shape
// PlanEnvRemoveSeam's EnvRemovalPlan already returns, reused here rather than
// inventing a second identical struct. See TeardownOptions.Planner for WHEN
// it may be called.
type TeardownPlanner func(name string) (EnvRemovalPlan, error)

// defaultTeardownPlanner is the byte-compatible default: the exact
// sandbox.PlanForceRemove(name) call every teardown planned before E2.5's
// injectable Planner existed, wrapped with an always-empty Report.
func defaultTeardownPlanner(name string) (EnvRemovalPlan, error) {
	argv, err := sandbox.PlanForceRemove(name)
	if err != nil {
		return EnvRemovalPlan{}, err
	}
	return EnvRemovalPlan{Argv: argv}, nil
}

func (o TeardownOptions) withDefaults() TeardownOptions {
	if o.RmTimeout <= 0 || o.RmTimeout > TeardownRmTimeout {
		o.RmTimeout = TeardownRmTimeout
	}
	if o.ProbeTimeout <= 0 || o.ProbeTimeout > TeardownProbeTimeout {
		o.ProbeTimeout = TeardownProbeTimeout
	}
	if o.ProbeRetries < 0 {
		o.ProbeRetries = TeardownProbeRetries
	}
	if o.Budget <= 0 || o.Budget > TeardownBudget {
		o.Budget = TeardownBudget
	}
	if o.ProbeDelay < 0 {
		o.ProbeDelay = teardownProbeDelay
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Planner == nil {
		o.Planner = defaultTeardownPlanner
	}
	return o
}

// TeardownResult is one attempt's outcome: what happened, and the sentence a
// caller prints or journals. Detail is always populated — a verdict with no
// reason is exactly what makes a reaper impossible to trust.
type TeardownResult struct {
	Sandbox string
	Key     string
	Trigger TeardownTrigger
	Verdict TeardownVerdict
	Detail  string
}

// Removed reports whether the SANDBOX is gone as of this attempt, by removal or
// by having been absent already — what a caller's exit code turns on.
func (r TeardownResult) Removed() bool {
	return r.Verdict == TeardownRemoved || r.Verdict == TeardownAlreadyAbsent
}

// String is the one-line rendering used for both the journal detail and the
// operator-facing note, so the two cannot describe one event differently.
func (r TeardownResult) String() string {
	return fmt.Sprintf("%s: %s (%s)", r.Sandbox, r.Verdict, r.Detail)
}

func TeardownSandbox(env hostenv.Env, key, name string, trigger TeardownTrigger, opts TeardownOptions) TeardownResult {
	o := opts.withDefaults()
	res := decideTeardown(env, key, name, trigger, o)
	res.Sandbox, res.Key, res.Trigger = name, key, trigger

	if res.Removed() {
		if uatRec, err := uat.ReadRegistration(env, name); err == nil && uatRec != nil {
			if uerr := uat.UnregisterMCP(env, uatRec.MCPName); uerr == nil {
				_ = uat.DeleteRegistration(env, name)
			} else {
				// Cleanup failure retains state, visible for retry.
			}
		}
	}

	if err := appendTeardownJournal(o, res); err != nil {
		// A journal that cannot be written must not change what happened to the
		// sandbox; it is reported through the result so the caller can warn.
		res.Detail += fmt.Sprintf(" (journal: %v)", err)
	}
	return res
}

func kept(v TeardownVerdict, format string, a ...any) TeardownResult {
	return TeardownResult{Verdict: v, Detail: fmt.Sprintf(format, a...)}
}

// decideTeardown is TeardownSandbox minus the journalling: scope first, then the
// non-blocking proof, then everything that may only be read UNDER it.
func decideTeardown(env hostenv.Env, key, name string, trigger TeardownTrigger, o TeardownOptions) TeardownResult {
	deadline := o.Now().Add(o.Budget)

	// pix-* scope FIRST — a FIXED guard, not the injectable o.Planner, so no
	// caller-supplied planner can widen scope or skip it: the SAME check
	// sandbox.PlanForceRemove/PlanRemove already run, exercised here purely
	// to refuse before any lease, probe, or proof work runs. The argv that
	// actually gets EXECUTED is composed separately, by o.Planner, only at
	// the two points below that have already cleared this domain's OWN
	// authority gate — never here.
	if _, perr := sandbox.PlanForceRemove(name); perr != nil {
		return kept(TeardownKeptUnowned, "%v", perr)
	}
	dir, derr := existingLeaseDir(key)
	if derr != nil {
		if trigger.requiresOwnership() {
			return kept(TeardownKeptUnowned, "no lease state for %q: %v", key, derr)
		}
		// An explicit rm of a box with no lease state has nothing to prove a
		// reference against and nothing to clear: remove it directly, bounded.
		// This IS the second authorized posture (an explicitly, individually
		// named removal intent with no lease state left to prove against), so
		// planning the executed argv here — not gated by TryReapProof, because
		// there is no proof left to take — is correct, not a bypass.
		plan, perr := o.Planner(name)
		if perr != nil {
			return kept(TeardownKeptUnowned, "%v", perr)
		}
		return removeAndConfirm(env, "", name, plan, o, deadline)
	}

	var res TeardownResult
	proofErr := lease.TryReapProof(dir, func() error {
		res = teardownUnderProof(env, dir, key, name, trigger, o, deadline)
		return nil
	})
	switch {
	case proofErr == nil:
		return res
	case errors.Is(proofErr, lease.ErrHeld):
		return kept(TeardownKeptBusy, "another shell still holds a live reference to %q (or a lifecycle transition is in flight)", name)
	default:
		return kept(TeardownKeptUnknown, "could not prove %q has zero live references: %v", name, proofErr)
	}
}

func teardownUnderProof(env hostenv.Env, dir, key, name string, trigger TeardownTrigger, o TeardownOptions, deadline time.Time) TeardownResult {
	rec, rerr := lease.ReadRecord(dir)
	if trigger.requiresOwnership() {
		switch {
		case rerr != nil:
			return kept(TeardownKeptUnowned, "%q has no valid creation record (%v) — this host cannot prove it created it", name, rerr)
		case lease.ValidateInstanceID(rec.InstanceID) != nil:
			return kept(TeardownKeptUnowned, "%q records an unusable instance id %q", name, rec.InstanceID)
		}
		if _, ok := readSessionFingerprint(key); !ok {
			return kept(TeardownKeptUnowned, "%q has no valid recorded fingerprint — its creation was never fully recorded", name)
		}
		if inv, ok := readSessionInvocation(key); !ok || len(inv) == 0 {
			return kept(TeardownKeptUnowned, "%q has no valid recorded pi invocation — its creation was never fully recorded", name)
		}
	}
	if trigger.honorsKeep() {
		state, set, kerr := lease.ReadKeep(dir)
		switch {
		case kerr != nil:
			// A keep we cannot read might be a keep. Fail closed.
			return kept(TeardownKeptUnknown, "refusing to reap: keep is held or unreadable for %q: %v", name, kerr)
		case set && strings.TrimSpace(state.Identity) != "":
			return kept(TeardownKeptKeep, "refusing to reap: keep is held by %q on %q", state.Identity, name)
		}
	}

	// The runtime's OWN answer, re-probed under the proof: the recorded
	// instance id must still be the instance living under that name.
	within := budgetedTimeout(o.ProbeTimeout, o, deadline)
	if within <= 0 {
		return kept(TeardownKeptUnknown, "teardown budget (%s) elapsed before %q could be identified", o.Budget, name)
	}
	entry, trusted := sbxEntry(env, name, within)
	switch {
	case !trusted:
		return kept(TeardownKeptUnknown, "could not trust `sbx ls --json` for %q; leaving it alone", name)
	case entry == nil:
		// Positively absent: nothing to remove, only stale state to clear.
		return clearedResult(TeardownAlreadyAbsent, dir, name,
			"%q is already gone; cleared its stale lease state", name)
	case !entry.IdentityVerified || entry.InstanceID == nil || *entry.InstanceID == "":
		return kept(TeardownKeptUnknown, "%q is listed without a schema-verified instance id; leaving it alone", name)
	case rerr == nil && *entry.InstanceID != rec.InstanceID:
		return kept(TeardownKeptMismatch, "%q now runs instance %q, not the recorded %q — the name was reused", name, *entry.InstanceID, rec.InstanceID)
	}

	// Every zero-holder/keep/instance-id/fresh-probe proof above has now
	// passed. Only from this point may the injected planner's argv reach
	// removeAndConfirm — and therefore env.RunWithin: teardownUnderProof is
	// itself only ever invoked from inside lease.TryReapProof's closure
	// above, so a proof failure (ErrHeld or otherwise) means this function,
	// and o.Planner with it, is never called at all.
	plan, perr := o.Planner(name)
	if perr != nil {
		return kept(TeardownKeptUnowned, "%v", perr)
	}
	return removeAndConfirm(env, dir, name, plan, o, deadline)
}

func removeAndConfirm(env hostenv.Env, dir, name string, plan EnvRemovalPlan, o TeardownOptions, deadline time.Time) TeardownResult {
	rmArgv := plan.Argv
	rmBudget := budgetedTimeout(o.RmTimeout, o, deadline)
	if rmBudget <= 0 {
		return kept(TeardownKeptUnknown, "teardown budget (%s) elapsed before %q could be removed", o.Budget, name)
	}
	out, timedOut, err := env.RunWithin(rmBudget, "sbx", rmArgv...)
	switch {
	case timedOut:
		return withPlanReport(TeardownResult{Verdict: TeardownFailed, Detail: fmt.Sprintf("`sbx %s` did not finish within %s; %q may still exist and its state is retained", strings.Join(rmArgv, " "), rmBudget, name)}, plan)
	case err != nil:
		return withPlanReport(TeardownResult{Verdict: TeardownFailed, Detail: fmt.Sprintf("`sbx %s` failed: %v: %s", strings.Join(rmArgv, " "), err, firstLine(out))}, plan)
	}

	for attempt := 0; attempt <= o.ProbeRetries; attempt++ {
		if attempt > 0 && o.ProbeDelay > 0 {
			time.Sleep(o.ProbeDelay)
		}
		within := budgetedTimeout(o.ProbeTimeout, o, deadline)
		if within <= 0 {
			break
		}
		if probeStateWithin(env, name, within) == SbxAbsent {
			return withPlanReport(clearedResult(TeardownRemoved, dir, name, "removed %q (pix's own zero-reference/explicit-intent gate, not sbx's -f) and confirmed it is gone", name), plan)
		}
	}
	return withPlanReport(TeardownResult{Verdict: TeardownFailed, Detail: fmt.Sprintf("`sbx %s` reported success but %q was never confirmed absent within %d probes; its state is retained", strings.Join(rmArgv, " "), name, o.ProbeRetries+1)}, plan)
}

// withPlanReport appends plan.Report (E2.4's fallback cleanup-not-run note,
// set only when PlanEnvRemoveSeam fell back to name-based removal) to res's
// Detail, so a caller sees it regardless of whether the removal itself
// succeeded or failed. Empty Report (the default planner, and the primary
// environment-scoped path) leaves Detail untouched.
func withPlanReport(res TeardownResult, plan EnvRemovalPlan) TeardownResult {
	if plan.Report != "" {
		res.Detail += fmt.Sprintf("; %s", plan.Report)
	}
	return res
}

// clearedResult clears the session's recorded state (launcher-owned files
// first, then everything package lease owns, then the directory) and reports v.
func clearedResult(v TeardownVerdict, dir, name, format string, a ...any) TeardownResult {
	res := TeardownResult{Verdict: v, Detail: fmt.Sprintf(format, a...)}
	if dir == "" {
		return res
	}
	if err := clearSessionState(dir); err != nil {
		res.Detail += fmt.Sprintf("; some lease state for %q could not be cleared: %v", name, err)
	}
	return res
}

func clearSessionState(dir string) error {
	var errs []error
	for _, name := range []string{sessionFingerprintFileName, sessionInvocationFileName} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
		// writeFileAtomic's temp names carry a random suffix (unlike the old
		// fixed ".tmp"), so a crash-leftover from a write that never reached
		// its rename needs a glob, not an exact name, to find and remove.
		leftover, _ := filepath.Glob(filepath.Join(dir, name+".tmp-*"))
		for _, p := range leftover {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	errs = append(errs, lease.ClearState(dir))
	return errors.Join(errs...)
}

func budgetedTimeout(step time.Duration, o TeardownOptions, deadline time.Time) time.Duration {
	remaining := deadline.Sub(o.Now())
	if remaining <= 0 {
		return 0
	}
	if step < remaining {
		return step
	}
	return remaining
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func existingLeaseDir(key string) (string, error) {
	root, err := leaseRoot()
	if err != nil {
		return "", err
	}
	dir, err := lease.SandboxDir(root, key)
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("launch: refusing to follow symlink at %s", dir)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("launch: %s is not a directory", dir)
	}
	return dir, nil
}

// ── the orphan sweep ────────────────────────────────────────────────────────

func orphanCandidates() ([]string, error) {
	root, err := leaseRoot()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var keys []string
	for _, e := range entries {
		if !e.IsDir() || lease.ValidateInstanceID(e.Name()) != nil {
			continue
		}
		keys = append(keys, e.Name())
	}
	return keys, nil
}

// sweepOrphans runs the automatic teardown over every lease-recorded session:
func sweepOrphans(env hostenv.Env, out io.Writer, opts TeardownOptions) ([]TeardownResult, error) {
	keys, err := orphanCandidates()
	if err != nil {
		return nil, fmt.Errorf("could not list lease state: %w", err)
	}
	var results []TeardownResult
	for _, key := range keys {
		res := TeardownSandbox(env, key, key, TriggerOrphanSweep, opts)
		results = append(results, res)
		fmt.Fprintf(out, "%s\n", res)
	}

	// Retry UAT registration cleanup
	if dir, err := uat.StateDir(env); err == nil {
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
					name := strings.TrimSuffix(e.Name(), ".json")
					// If lease dir is gone, we should cleanup UAT
					if _, lerr := existingLeaseDir(name); lerr != nil {
						if uatRec, err := uat.ReadRegistration(env, name); err == nil && uatRec != nil {
							if uerr := uat.UnregisterMCP(env, uatRec.MCPName); uerr == nil {
								_ = uat.DeleteRegistration(env, name)
							}
						}
					}
				}
			}
		}
	}

	if len(results) == 0 {
		fmt.Fprintln(out, "No pix sandboxes with lease state to sweep.")
	}
	return results, nil
}

// ── the journal ─────────────────────────────────────────────────────────────

const (
	TeardownJournalName       = "teardown.jsonl"
	TeardownJournalMaxEntries = 200
	teardownJournalMaxBytes   = 64 * 1024
)

type TeardownJournalEntry struct {
	At      time.Time       `json:"at"`
	Sandbox string          `json:"sandbox"`
	Key     string          `json:"key"`
	Trigger TeardownTrigger `json:"trigger"`
	Verdict TeardownVerdict `json:"verdict"`
	Detail  string          `json:"detail,omitempty"`
}

func teardownJournalPath() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, TeardownJournalName), nil
}

// teardownJournalGuardTimeout bounds the flock below. It is NOT a liveness
// signal — it only serializes a fast local read-modify-write, the same
// posture as lease's keepGuardTimeout — so a short, fixed bound is correct:
// a caller stuck longer than this is stuck on something else entirely.
//
// 2s was too tight for the CONTENTION this actually sees. A non-blocking poll
// has no queue: 40 concurrent appenders (the shape the test exercises, and the
// shape a sweep plus several session exits produce) all retry on the same
// cadence, collide, and retry again, so the unluckiest writer can starve while
// the file is never held for more than a moment. It failed exactly that way on
// a loaded CI runner while passing on every developer laptop.
const teardownJournalGuardTimeout = 15 * time.Second

// journalGuardPoll is the retry cadence, and journalGuardJitter breaks the
// lockstep that makes polling starve. Without jitter every waiter wakes on the
// same 5ms boundary and the same loser loses repeatedly; with it, the retries
// spread out and the queue drains. This is the fix — the longer budget above
// only buys headroom for a pathological case.
const (
	journalGuardPoll   = 5 * time.Millisecond
	journalGuardJitter = 4 * time.Millisecond
)

// withTeardownJournalGuard serializes the journal's read-modify-write across
// PROCESSES: an orphan sweep and a session's own teardown are frequently two
// different `pix-host` invocations writing the SAME journal path, and a
// unique temp name (writeFileAtomic) only stops one writer's rename from
// landing HALF another's data — it does nothing about two writers each
// reading the file's prior lines before either has appended its own, where
// the slower writer's rewrite silently drops the faster one's entry. The
// guard is a flock on a dedicated lock file next to the journal.
func withTeardownJournalGuard(path string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	deadline := time.Now().Add(teardownJournalGuardTimeout)
	for {
		ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if ferr == nil {
			break
		}
		if !errors.Is(ferr, syscall.EWOULDBLOCK) && !errors.Is(ferr, syscall.EAGAIN) {
			return &os.PathError{Op: "flock", Path: lockPath, Err: ferr}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("launch: timed out waiting for the teardown journal lock at %s (another teardown is writing it)", lockPath)
		}
		time.Sleep(journalGuardPoll + time.Duration(rand.Int64N(int64(journalGuardJitter))))
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func appendTeardownJournal(o TeardownOptions, res TeardownResult) error {
	path := o.JournalPath
	if path == "" {
		p, err := teardownJournalPath()
		if err != nil {
			return err
		}
		path = p
	}
	line, err := json.Marshal(TeardownJournalEntry{
		At: o.Now().UTC(), Sandbox: res.Sandbox, Key: res.Key,
		Trigger: res.Trigger, Verdict: res.Verdict, Detail: res.Detail,
	})
	if err != nil {
		return err
	}
	return withTeardownJournalGuard(path, func() error {
		cleanStaleJournalTemp(path)
		lines := append(readJournalLines(path), string(line))
		if len(lines) > TeardownJournalMaxEntries {
			lines = lines[len(lines)-TeardownJournalMaxEntries:]
		}
		for len(lines) > 1 && journalBytes(lines) > teardownJournalMaxBytes {
			lines = lines[1:]
		}
		return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
	})
}

// cleanStaleJournalTemp removes any writeFileAtomic temp file left behind by a
// PRIOR writer of this same journal that crashed between its write and its
// rename (writeFileAtomic's own defer only cleans up the temp it just made,
// within ITS OWN call). It is only ever called from inside
// withTeardownJournalGuard, which is what makes the glob safe to remove
// outright rather than merely report: holding that lock excludes every other
// writer of this path, so any "<journal>.tmp-*" sibling found here cannot be a
// write in flight — it can only be leftover debris from one that never
// finished, the same crash-recovery posture clearSessionState already applies
// to the fingerprint/invocation files next door.
func cleanStaleJournalTemp(path string) {
	leftover, _ := filepath.Glob(path + ".tmp-*")
	for _, p := range leftover {
		_ = os.Remove(p)
	}
}

func journalBytes(lines []string) int {
	n := 0
	for _, l := range lines {
		n += len(l) + 1
	}
	return n
}

func readJournalLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
