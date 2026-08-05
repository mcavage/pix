//go:build unix

// reap.go — U04d's LAST-SHELL TEARDOWN and the orphan reaper: the half of the
// sandbox lifecycle U04c2 deliberately left out (see session.go's header).
//
// The whole safety argument, in the order it is enforced:
//
//	this shell's refs SH is CLOSED first  ->  lease.TryReapProof (lifecycle EX
//	+ refs EX, both NON-BLOCKING: zero live references, no transition in
//	flight)  ->  under that proof: a valid creation record, fingerprint and
//	invocation; the SAME immutable instance id re-probed from the runtime; no
//	valid identity-bound keep; a pix-* name this domain owns  ->  a NON-FORCE
//	`sbx rm <name>` (bounded)  ->  a POSITIVE absent probe  ->  only then the
//	lease/keep/fingerprint/invocation state is cleared.
//
// Every weaker answer — a held reference, a held keep, an untrusted probe, an
// instance id that no longer matches the record, a name outside pix-* — KEEPS
// the sandbox and journals why. There is no `sbx rm -f` in this file at all:
// only lease.TryExclusive()'s kernel-verified zero-holder proof (via
// lease.TryReapProof) authorizes destroying sandbox state, and force is
// reserved for an explicitly-named `pix rm --force` typed by a human (see
// sandbox.go's RemovePixSandbox).
//
// MCP receipts are deliberately NOT touched here: a removed sandbox's receipt
// describes a dead lifetime, but reaping receipts is its own slice, and the
// next create's pre-create clear is the correctness backstop either way. This
// file never deletes one. That has a VISIBLE consequence worth knowing before
// reading the tests: workspace.MCPStateRoot is ALSO <state>/sandboxes/<name>,
// so a real session's receipt (mcp.json + mcp.json.lock) lives in the very
// directory the lease state does. lease.ClearState only removes the directory
// when nothing it does not own is left in it, so after a successful teardown
// the identity/ownership files are gone and the directory REMAINS, holding the
// retained receipt. That is the intended boundary, not a leak.
package launch

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/lease"
	"pix/host/sandbox"
)

// The teardown budget, as three bounds that compose to one ceiling:
//
//   - TeardownRmTimeout bounds the single mutating `sbx rm` call;
//   - TeardownProbeTimeout bounds ONE absent/identity probe, and
//     TeardownProbeRetries is how many EXTRA absent probes follow the first;
//   - TeardownBudget is the absolute wall-clock ceiling for the whole attempt,
//     and it is what makes the arithmetic safe: 15s + 3x3s = 24s of nominal
//     step budget is CLAMPED to 20s, because a session's exit must never hang
//     on a teardown. Every step takes min(its own bound, what is left).
const (
	TeardownRmTimeout    = 15 * time.Second
	TeardownProbeTimeout = 3 * time.Second
	TeardownProbeRetries = 2
	TeardownBudget       = 20 * time.Second
	// teardownProbeDelay spaces the absent re-probes: a runtime that reports
	// its own removal asynchronously needs a beat, and 3 probes back-to-back
	// inside one RTT would prove nothing the first already did.
	teardownProbeDelay = 250 * time.Millisecond
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
	// TeardownKeptBusy: another shell still holds a live reference, or a
	// lifecycle transition is in flight. The proof is non-blocking by design.
	TeardownKeptBusy TeardownVerdict = "kept-busy"
	// TeardownKeptKeep: an identity-bound keep is held (`pix run -k`).
	TeardownKeptKeep TeardownVerdict = "kept-keep"
	// TeardownKeptUnowned: no valid creation record / fingerprint /
	// invocation, or a name outside pix-*. Nothing here can prove this host
	// created that sandbox, so nothing here may remove it.
	TeardownKeptUnowned TeardownVerdict = "kept-unowned"
	// TeardownKeptMismatch: the runtime's instance id for that name is not the
	// one recorded — the name was reused for a DIFFERENT instance.
	TeardownKeptMismatch TeardownVerdict = "kept-mismatch"
	// TeardownKeptUnknown: a probe could not be trusted, the budget ran out,
	// or state was unreadable. Fail closed: keep it.
	TeardownKeptUnknown TeardownVerdict = "kept-unknown"
	// TeardownFailed: the removal itself failed, or succeeded without the
	// sandbox's absence ever being confirmed — state is RETAINED, because an
	// unknown removal outcome must keep its evidence.
	TeardownFailed TeardownVerdict = "failed"
)

// TeardownTrigger is what asked for the teardown, and it decides the two
// permissions that differ between the automatic and the explicit paths.
type TeardownTrigger string

const (
	// TriggerSession is the last shell out of a session (RunSession) and
	// TriggerOrphanSweep is `pix rm --orphans`. Both are AUTOMATIC: they
	// require positive ownership and they honor a keep.
	TriggerSession     TeardownTrigger = "session"
	TriggerOrphanSweep TeardownTrigger = "orphan-sweep"
	// TriggerExplicit is `pix rm <name>` typed by a human. It still requires
	// the zero-reference proof — an explicit rm of a sandbox somebody else is
	// using is refused, and `--force` is the only way past that — but it does
	// NOT require ownership (a legacy or never-recorded box must remain
	// removable) and it does not defer to a keep: a keep exists to stop the
	// AUTOMATIC reaper, not to argue with the operator.
	TriggerExplicit TeardownTrigger = "explicit"
)

// requiresOwnership / honorsKeep are the trigger's two permissions, kept as
// methods so there is one place that answers "what may this trigger do".
func (t TeardownTrigger) requiresOwnership() bool { return t != TriggerExplicit }
func (t TeardownTrigger) honorsKeep() bool        { return t != TriggerExplicit }

// TeardownOptions are a teardown's bounds and collaborators. Its zero value is
// the production shape (see withDefaults) so a caller that has no opinion
// passes TeardownOptions{}.
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

// Removed reports whether the SANDBOX is gone as of this attempt, by removal
// or by having been absent already — the one question a caller's exit code and
// its "removed N" summary both turn on.
func (r TeardownResult) Removed() bool {
	return r.Verdict == TeardownRemoved || r.Verdict == TeardownAlreadyAbsent
}

// String is the one-line rendering used for both the journal detail and the
// operator-facing note, so the two can never describe the same event
// differently.
func (r TeardownResult) String() string {
	return fmt.Sprintf("%s: %s (%s)", r.Sandbox, r.Verdict, r.Detail)
}

// TeardownSandbox is the whole teardown decision for ONE sandbox, and the only
// entry point: last-shell teardown, the orphan sweep and explicit `pix rm` all
// go through it so the ordering above exists once. It never returns an error —
// every outcome is a verdict, because "could not decide" is itself a decision
// (keep it) — and it journals every attempt.
//
// key is the lease identity (SessionName); name is the actual sbx sandbox name.
func TeardownSandbox(env hostenv.Env, key, name string, trigger TeardownTrigger, opts TeardownOptions) TeardownResult {
	o := opts.withDefaults()
	res := decideTeardown(env, key, name, trigger, o)
	res.Sandbox, res.Key, res.Trigger = name, key, trigger
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

// decideTeardown is TeardownSandbox minus the journalling: scope first, then
// the non-blocking proof, then everything that may only be read UNDER it.
func decideTeardown(env hostenv.Env, key, name string, trigger TeardownTrigger, o TeardownOptions) TeardownResult {
	deadline := o.Now().Add(o.Budget)

	// pix-* scope FIRST, and through the planner that composes the argv:
	// PlanRemove refuses a name outside this domain's namespace AND yields a
	// NON-FORCE `rm <name>` by construction, so there is no path here that can
	// compose a forced removal even by mistake.
	rmArgv, perr := sandbox.PlanRemove(name)
	if perr != nil {
		return kept(TeardownKeptUnowned, "%v", perr)
	}
	dir, derr := existingLeaseDir(key)
	if derr != nil {
		if trigger.requiresOwnership() {
			return kept(TeardownKeptUnowned, "no lease state for %q: %v", key, derr)
		}
		// An explicit rm of a box with no lease state has nothing to prove a
		// reference against and nothing to clear: remove it directly, bounded.
		return removeAndConfirm(env, "", name, rmArgv, o, deadline)
	}

	var res TeardownResult
	proofErr := lease.TryReapProof(dir, func() error {
		res = teardownUnderProof(env, dir, key, name, rmArgv, trigger, o, deadline)
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

// teardownUnderProof runs with BOTH locks held: lifecycle EX (no create/attach
// can interleave) and refs EX (zero live references). Everything it reads is
// therefore the truth for as long as it acts on it.
func teardownUnderProof(env hostenv.Env, dir, key, name string, rmArgv []string, trigger TeardownTrigger, o TeardownOptions, deadline time.Time) TeardownResult {
	rec, rerr := lease.ReadRecord(dir)
	if trigger.requiresOwnership() {
		switch {
		case rerr != nil:
			return kept(TeardownKeptUnowned, "%q has no valid creation record (%v) — this host cannot prove it created it", name, rerr)
		case lease.ValidateInstanceID(rec.InstanceID) != nil:
			return kept(TeardownKeptUnowned, "%q records an unusable instance id %q", name, rec.InstanceID)
		}
		if _, ok := ReadSessionFingerprint(key); !ok {
			return kept(TeardownKeptUnowned, "%q has no valid recorded fingerprint — its creation was never fully recorded", name)
		}
		if inv, ok := ReadSessionInvocation(key); !ok || len(inv) == 0 {
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
	entry, trusted := probeInstance(env, name, budgetedTimeout(o.ProbeTimeout, o, deadline))
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
	return removeAndConfirm(env, dir, name, rmArgv, o, deadline)
}

// removeAndConfirm performs the NON-FORCE removal and proves it: `sbx rm
// <name>` bounded by the smaller of RmTimeout and what is left of the budget,
// then up to 1+ProbeRetries absent probes, each bounded the same way. State is
// cleared ONLY after a POSITIVE absent answer; anything less keeps it.
func removeAndConfirm(env hostenv.Env, dir, name string, rmArgv []string, o TeardownOptions, deadline time.Time) TeardownResult {
	rmBudget := budgetedTimeout(o.RmTimeout, o, deadline)
	if rmBudget <= 0 {
		return kept(TeardownKeptUnknown, "teardown budget (%s) elapsed before %q could be removed", o.Budget, name)
	}
	out, timedOut, err := env.RunWithin(rmBudget, "sbx", rmArgv...)
	switch {
	case timedOut:
		return TeardownResult{Verdict: TeardownFailed, Detail: fmt.Sprintf("`sbx %s` did not finish within %s; %q may still exist and its state is retained", strings.Join(rmArgv, " "), rmBudget, name)}
	case err != nil:
		return TeardownResult{Verdict: TeardownFailed, Detail: fmt.Sprintf("`sbx %s` failed: %v: %s", strings.Join(rmArgv, " "), err, firstLine(out))}
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
			return clearedResult(TeardownRemoved, dir, name, "removed %q (no force) and confirmed it is gone", name)
		}
	}
	return TeardownResult{Verdict: TeardownFailed, Detail: fmt.Sprintf("`sbx %s` reported success but %q was never confirmed absent within %d probes; its state is retained", strings.Join(rmArgv, " "), name, o.ProbeRetries+1)}
}

// clearedResult clears the session's recorded state (launcher-owned files
// first, then everything package lease owns, then the directory) and reports
// v. A clear that partially fails is reported in the detail rather than
// downgrading the verdict: the SANDBOX outcome is already settled by the time
// this runs, and pretending otherwise would be the dishonest half.
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

// clearSessionState removes the two launcher-owned records (fingerprint and
// invocation, written by session.go alongside the lease) and then hands the
// rest to lease.ClearState, which owns the record, the keep and the locks.
// Order matters: lease.ClearState only removes the DIRECTORY when nothing it
// does not own is left in it.
func clearSessionState(dir string) error {
	var errs []error
	for _, name := range []string{
		sessionFingerprintFileName, sessionFingerprintFileName + ".tmp",
		sessionInvocationFileName, sessionInvocationFileName + ".tmp",
	} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	errs = append(errs, lease.ClearState(dir))
	return errors.Join(errs...)
}

// budgetedTimeout is the step bound that keeps the composed ceiling honest:
// the smaller of the step's own timeout and whatever is left of the total
// budget. A non-positive answer means "out of budget", never "unbounded".
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

// probeInstance asks the runtime for name's row, bounded. trusted=false is
// every "could not learn anything" answer (sbx failed, timed out, unparseable
// output); trusted=true with a nil entry is the POSITIVE "it is not there",
// which is the only absence this file acts on.
func probeInstance(env hostenv.Env, name string, within time.Duration) (entry *sandbox.Entry, trusted bool) {
	if within <= 0 {
		return nil, false
	}
	out, timedOut, err := env.RunWithin(within, "sbx", "ls", "--json")
	if timedOut || err != nil {
		return nil, false
	}
	parsed, perr := sandbox.ParseList([]byte(out))
	if perr != nil {
		return nil, false
	}
	return sandbox.FindByName(parsed.Entries, name), true
}

// probeStateWithin is ProbeTaskSandbox on a caller-chosen bound — the absent
// confirmation needs a TIGHTER timeout than the default probe, and it shares
// classifySbxListing so the two can never disagree about what a row means.
func probeStateWithin(env hostenv.Env, name string, within time.Duration) SbxState {
	out, timedOut, err := env.RunWithin(within, "sbx", "ls")
	if timedOut || err != nil {
		return SbxUnknown
	}
	return classifySbxListing(out, name)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// existingLeaseDir resolves key's lease directory WITHOUT creating it (unlike
// LeaseDirFor, which is a create path): a teardown that conjured the directory
// it is about to inspect would manufacture the very "no state" answer it is
// trying to read.
func existingLeaseDir(key string) (string, error) {
	root, err := LeaseRoot()
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

// OrphanCandidates lists the session keys this host has lease state for, in
// listing order. It is the ONLY discovery source for the orphan sweep, and
// that is the point: a candidate exists because THIS host recorded creating
// it, so the sweep can never reach a sandbox it cannot prove ownership of. It
// deliberately does not consult `sbx ls` — a box with no lease state is not an
// orphan this tool may collect, it is somebody else's.
func OrphanCandidates() ([]string, error) {
	root, err := LeaseRoot()
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

// SweepOrphans runs the automatic teardown over every lease-recorded session:
// positive ownership and a kernel-verified zero-reference proof only, never
// force, keeps honored. A candidate whose sandbox is still referenced, kept, or
// unprovable is left exactly where it is and journalled.
//
// The session key IS the default sandbox name (SessionName == sandbox.Name), so
// a sandbox created with an explicit --name is not swept: its box name is not
// derivable from the lease key, and guessing it is precisely the "invented
// identity" this lifecycle refuses.
func SweepOrphans(env hostenv.Env, out io.Writer, opts TeardownOptions) ([]TeardownResult, error) {
	keys, err := OrphanCandidates()
	if err != nil {
		return nil, fmt.Errorf("could not list lease state: %w", err)
	}
	var results []TeardownResult
	for _, key := range keys {
		res := TeardownSandbox(env, key, key, TriggerOrphanSweep, opts)
		results = append(results, res)
		fmt.Fprintf(out, "%s\n", res)
	}
	if len(results) == 0 {
		fmt.Fprintln(out, "No pix sandboxes with lease state to sweep.")
	}
	return results, nil
}

// ── the journal ─────────────────────────────────────────────────────────────

// The journal is a BOUNDED, 0600 JSONL file in the STATE dir (never the config
// dir — AGENTS.md safety invariant #4): every teardown attempt, including every
// refusal, with the reason. It is capped in BOTH directions a log can grow
// (entries and bytes) and rewritten tail-first, so a host that reaps hundreds
// of sandboxes cannot turn an audit trail into an unbounded disk consumer.
const (
	TeardownJournalName       = "teardown.jsonl"
	TeardownJournalMaxEntries = 200
	teardownJournalMaxBytes   = 64 * 1024
)

// TeardownJournalEntry is one line of the journal.
type TeardownJournalEntry struct {
	At      time.Time       `json:"at"`
	Sandbox string          `json:"sandbox"`
	Key     string          `json:"key"`
	Trigger TeardownTrigger `json:"trigger"`
	Verdict TeardownVerdict `json:"verdict"`
	Detail  string          `json:"detail,omitempty"`
}

// TeardownJournalPath is <state>/teardown.jsonl.
func TeardownJournalPath() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, TeardownJournalName), nil
}

// appendTeardownJournal appends res and re-bounds the file. The whole journal
// is rewritten through a 0600 temp file + rename on every append: at
// TeardownJournalMaxEntries lines that is trivially cheap, it makes the bound
// exact rather than eventual, and the rename REPLACES a symlink at the path
// instead of writing through it.
func appendTeardownJournal(o TeardownOptions, res TeardownResult) error {
	path := o.JournalPath
	if path == "" {
		p, err := TeardownJournalPath()
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
	lines := append(readJournalLines(path), string(line))
	if len(lines) > TeardownJournalMaxEntries {
		lines = lines[len(lines)-TeardownJournalMaxEntries:]
	}
	for len(lines) > 1 && journalBytes(lines) > teardownJournalMaxBytes {
		lines = lines[1:]
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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

// ReadTeardownJournal decodes the journal at path, oldest first. A line that
// does not decode is SKIPPED rather than failing the read: a journal is
// evidence, and one corrupt tail line must not hide the other 199.
func ReadTeardownJournal(path string) ([]TeardownJournalEntry, error) {
	if path == "" {
		p, err := TeardownJournalPath()
		if err != nil {
			return nil, err
		}
		path = p
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var entries []TeardownJournalEntry
	for _, l := range readJournalLines(path) {
		var e TeardownJournalEntry
		if json.Unmarshal([]byte(l), &e) == nil {
			entries = append(entries, e)
		}
	}
	return entries, nil
}
