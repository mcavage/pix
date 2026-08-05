//go:build unix

// session.go — U04c2's CREATE/ATTACH-ONLY slice of the sandbox lifecycle,
// built on the two packages that already own the primitives: pix/host/lease
// (the immutable creation record, the identity-bound keep, and — since U04c1
// — the SEPARATE lifecycle.lock/refs.lock pair and the AttachRef ordering
// helpers that compose them) and pix/host/sandbox (the deterministic digest
// name, the tolerant `sbx ls` parse, create-vs-exec argv, the fingerprint
// diff).
//
// The ordering this file exists to get right, once, in one place:
//
//	lifecycle EX  ->  re-probe the runtime under it  ->  start the child
//	              ->  (create) poll until visible, then record instance id,
//	                  fingerprint, invocation and the MCP receipt
//	              ->  refs SH (still under lifecycle EX)
//	              ->  lifecycle UNLOCK  ->  wait for the session to exit
//
// Two properties are load-bearing and are what the process tests prove:
// everything a later attach or a future reaper reads is on disk BEFORE any
// other process can observe the transition as finished (so a killed creator
// still leaves a complete record), and the lifecycle lock covers only the
// TRANSITION — never the session's own lifetime — so a second attach against
// a live session is not serialized behind it.
//
// Teardown is U04d's, and it lives next door in reap.go: this file's last act
// is to CLOSE this shell's reference and hand the decision to
// TeardownSandbox, which owns the whole "may this be removed" argument. Nothing
// in this file plans or executes a removal itself — see RecordSessionCreation's
// doc for why an attach to a sandbox this host never recorded creating can
// never authorize one by accident.
package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/lease"
	"pix/host/mcp"
	"pix/host/sandbox"
)

// SessionLockTimeout bounds the wait for the per-session LIFECYCLE lock — the
// short wait for another process's create/attach transition to finish, not a
// session's lifetime. A session that cannot get it in this window reports
// that honestly instead of blocking an interactive launch forever. A CONSTANT,
// not a var: workflow/launch declares no mutable globals (see arch_test.go's
// TestLaunch_NoPackageLevelVars).
const SessionLockTimeout = 30 * time.Second

// LifecycleIdentity is the STICKY identity a keep is bound to: $PIX_IDENTITY
// when set (so a test or a CI runner can pin one), else user@host. It is
// deliberately stable across invocations — a keep set by one run must still
// be recognized as MINE by a later one — and it is never a PID: PIDs are
// recycled, and lease.Record.CreatedPID is advisory only.
func LifecycleIdentity() string {
	if id := strings.TrimSpace(os.Getenv("PIX_IDENTITY")); id != "" {
		return id
	}
	name := "unknown"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	return name + "@" + host
}

// LeaseRoot is the per-host lease state root, <state>/sandboxes. It lives in
// the STATE dir (AGENTS.md safety invariant #4) so `pix reset` moving the
// CONFIG dir aside can never orphan a live session's lease.
func LeaseRoot() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "sandboxes"), nil
}

// SessionName is the deterministic, digest-based identity a session's lease
// state is keyed by: sandbox.Name(workspace), e.g. "pix-myproj-a1b2c3d4" —
// the SAME name `pix run` gives the sandbox itself by default (see cmd/pix's
// resolveSandboxName), so the lease directory and the box agree. Two
// different workspaces that happen to share a basename never collide; the
// same workspace always resolves to the same lease dir. An explicit --name is
// the one case where the box's display name and this key differ, and that is
// deliberate: the lease tracks a WORKSPACE's session, not a display name.
func SessionName(workspace string) string { return sandbox.Name(workspace) }

// LeaseDirFor resolves (and creates, 0700) the lease directory for a session
// key (see SessionName). The ROOT is created here (0700, MkdirAll) rather
// than by lease: package lease deliberately does a single-level os.Mkdir so
// it can never conjure a path outside a root its caller vouched for, and on
// a fresh host <state>/sandboxes does not exist yet.
func LeaseDirFor(sessionKey string) (string, error) {
	root, err := LeaseRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("launch: create lease root: %w", err)
	}
	dir, err := lease.SandboxDir(root, sessionKey)
	if err != nil {
		return "", err
	}
	if err := lease.EnsureSandboxDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// SetSessionKeep binds a sticky, identity-bound keep marker to sessionKey:
// -k/--keep's whole effect. Since U04d it is no longer inert — it is exactly
// what the last-shell teardown and the orphan sweep refuse on (reap.go's
// TeardownKeptKeep). It still does NOT stop an explicitly named `pix rm`: a
// keep argues with the reaper, never with the operator.
func SetSessionKeep(sessionKey string) error {
	dir, err := LeaseDirFor(sessionKey)
	if err != nil {
		return err
	}
	return lease.SetKeep(dir, LifecycleIdentity())
}

// SessionFingerprint captures the identity-relevant facets of a launch that
// must agree between create and attach: the resolved create-time preloaded
// MCP set and the pinned image/template. Deliberately EXCLUDES anything
// create-only-heavy (kit resolution, --dev checkout) — WillCreate exists
// specifically so a plain re-attach never re-resolves those, and this
// fingerprint must be cheaply recomputable on every attach without doing so.
func SessionFingerprint(cfg *config.Config, o RunOpts) sandbox.Fingerprint {
	mcpSet := o.StaticMCP
	if len(mcpSet) == 0 && cfg != nil {
		mcpSet = mcp.AllPreloadedMCP(append(append([]string(nil), cfg.MCP...), o.MCP...))
	}
	sorted := append([]string(nil), mcpSet...)
	sort.Strings(sorted)
	fp := sandbox.Fingerprint{"static_mcp": strings.Join(sorted, ",")}
	switch {
	case o.Template != "":
		fp["template"] = o.Template
	case o.LocalImageTag != "":
		fp["template"] = o.LocalImageTag
	}
	return fp
}

// The fingerprint and the exact pi invocation are recorded ALONGSIDE the
// lease record (same dir, same 0600 mode), deliberately not inside package
// lease itself: both are launcher-shaped, caller-defined detail (see
// sandbox.Fingerprint's own doc), not lease's identity primitive.
const (
	sessionFingerprintFileName = "fingerprint.json"
	sessionInvocationFileName  = "invocation.json"
)

// writeSessionState writes v as JSON to name inside sessionKey's lease dir,
// 0600, temp+rename, so a reader never observes a partial file. Shared by the
// fingerprint and the invocation: two files, one write path.
func writeSessionState(sessionKey, name string, v any) error {
	dir, err := LeaseDirFor(sessionKey)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("launch: marshal %s: %w", name, err)
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("launch: write %s: %w", name, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("launch: commit %s: %w", name, err)
	}
	return nil
}

// readSessionState decodes name from sessionKey's lease dir. found=false
// covers every "nothing to compare against" case identically — no lease dir,
// no file, or a CORRUPT one — because the caller's response to all three is
// the same: attach UNOWNED rather than refuse (see ReadSessionInvocation and
// CheckSessionFingerprint).
func readSessionState(sessionKey, name string, v any) bool {
	dir, err := LeaseDirFor(sessionKey)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// WriteSessionFingerprint stores fp for sessionKey.
func WriteSessionFingerprint(sessionKey string, fp sandbox.Fingerprint) error {
	return writeSessionState(sessionKey, sessionFingerprintFileName, fp)
}

// CheckSessionFingerprint compares sessionKey's STORED fingerprint (recorded
// at creation) against current, returning the diverged keys (see
// sandbox.Diff). A MISSING or CORRUPT stored fingerprint — nothing was ever
// recorded, e.g. a legacy sandbox that predates this file, or one this host
// never proved it created — returns (nil, false): there is nothing to compare
// against, so the caller attaches UNOWNED rather than refusing. found=true
// with a non-empty result means an ACTUAL divergence a caller should refuse
// on.
func CheckSessionFingerprint(sessionKey string, current sandbox.Fingerprint) (diverged []string, found bool) {
	var stored sandbox.Fingerprint
	if !readSessionState(sessionKey, sessionFingerprintFileName, &stored) {
		return nil, false
	}
	return sandbox.Diff(stored, current), true
}

// ReadSessionFingerprint returns sessionKey's RECORDED create-time
// fingerprint. ok=false covers every "nothing usable was recorded" case —
// missing, corrupt, or decoded-but-empty — which is what U04d's teardown
// needs: an incompletely recorded creation is not ownership, so it authorizes
// no removal (see reap.go's teardownUnderProof).
func ReadSessionFingerprint(sessionKey string) (sandbox.Fingerprint, bool) {
	var fp sandbox.Fingerprint
	if !readSessionState(sessionKey, sessionFingerprintFileName, &fp) || len(fp) == 0 {
		return nil, false
	}
	return fp, true
}

// WriteSessionInvocation stores invocation (the create-time pi argv from
// BuildPiInvocation) for sessionKey.
func WriteSessionInvocation(sessionKey string, invocation []string) error {
	return writeSessionState(sessionKey, sessionInvocationFileName, invocation)
}

// ReadSessionInvocation returns sessionKey's stored create-time invocation.
// found=false whenever nothing usable was ever recorded — a legacy sandbox,
// one this host never proved it created (see RecordSessionCreation), or a
// corrupt file — in which case the caller must fall back to a freshly
// recomputed "safe current/default invocation" (BuildPiInvocation) rather
// than refuse the attach, exactly mirroring CheckSessionFingerprint's
// missing-record posture.
func ReadSessionInvocation(sessionKey string) (invocation []string, found bool) {
	if !readSessionState(sessionKey, sessionInvocationFileName, &invocation) {
		return nil, false
	}
	return invocation, true
}

// sbxJSONEntry runs `sbx ls --json` and looks up name in it, returning nil on
// ANY probe failure (sbx unavailable, invalid JSON) rather than an error —
// callers here already treat "could not learn anything" and "learned it's
// absent" identically (fail-closed towards NOT trusting the row), so there is
// nothing for an error return to add. Shared by RecordSessionCreation and
// FindPositivelyIdentifiedRunning so the two never parse `sbx ls --json`
// two different ways.
func sbxJSONEntry(env hostenv.Env, name string) *sandbox.Entry {
	out, err := env.Run("sbx", "ls", "--json")
	if err != nil {
		return nil
	}
	parsed, err := sandbox.ParseList([]byte(out))
	if err != nil {
		return nil
	}
	return sandbox.FindByName(parsed.Entries, name)
}

// FindPositivelyIdentifiedRunning reports the RUNNING, schema-verified `sbx
// ls --json` row for name, or ok=false for anything less certain (absent,
// stopped, unverified, or the probe itself failed) — the same "never guess"
// posture sandbox.PlanLaunch already holds for its own create-vs-exec
// decision. A caller uses ok==true to decide an ATTACH may use `sbx exec`
// (BuildAttachArgv) instead of the legacy `sbx run --name` re-attach, which
// remains the only path that can also restart a STOPPED sandbox or attach to
// one this row's own schema could not vouch for.
func FindPositivelyIdentifiedRunning(env hostenv.Env, name string) (*sandbox.Entry, bool) {
	found := sbxJSONEntry(env, name)
	if found == nil || !found.IdentityVerified || found.State != sandbox.StateRunning {
		return nil, false
	}
	return found, true
}

// RecordSessionCreation is the create half of U04c2, called ONLY after a
// POSITIVE poll has seen the new sandbox appear (see RunSession, which calls
// it while still holding the lifecycle lock): it re-derives the runtime's own
// listing, and writes the immutable instance record, the fingerprint, and the
// create-time pi invocation ONLY when that listing POSITIVELY names an
// instance id for name. A listing that fails to run, or answers but names no
// instance id, records NOTHING — the sandbox stays attachable, but forever
// UNOWNED, because a record whose instance id we invented would authorize a
// future teardown we cannot justify.
//
// invocation is the pi argv this exact create sent (BuildPiInvocation) —
// stored so a LATER attach can replay it verbatim via `sbx exec ... pi
// <invocation>` (BuildAttachArgv) instead of asking sbx to re-derive one.
//
// recorded reports whether an instance id was positively recorded, which is
// the only state in which -k/--keep may bind a keep (a keep on a session with
// no recorded identity would be a marker about nothing). A non-nil error must
// never turn a successful launch into a failed one — the sandbox already
// exists by the time this runs — but it IS returned (never printed by this
// package directly) so the caller can warn.
func RecordSessionCreation(env hostenv.Env, sessionKey, name string, fp sandbox.Fingerprint, invocation []string) (recorded bool, err error) {
	found := sbxJSONEntry(env, name)
	if found == nil || !found.IdentityVerified || found.InstanceID == nil || *found.InstanceID == "" {
		return false, nil // present but unowned: no verifiable instance id to record
	}
	dir, err := LeaseDirFor(sessionKey)
	if err != nil {
		return false, err
	}
	if _, err := lease.CreateRecord(dir, *found.InstanceID); err != nil {
		return false, fmt.Errorf("launch: could not record %s's creation lease: %w", name, err)
	}
	if err := WriteSessionFingerprint(sessionKey, fp); err != nil {
		return false, fmt.Errorf("launch: could not record %s's session fingerprint: %w", name, err)
	}
	if err := WriteSessionInvocation(sessionKey, invocation); err != nil {
		return false, fmt.Errorf("launch: could not record %s's session invocation: %w", name, err)
	}
	return true, nil
}

// SessionRecorded reports whether sessionKey has a readable immutable
// creation record — i.e. whether this host ever positively identified the
// instance it is attaching to. It is the gate on binding a keep: a keep on a
// session with no recorded identity would be a marker about nothing, and
// (being identity-bound) would have to be honored by a future reaper that
// cannot tell which sandbox it referred to.
func SessionRecorded(sessionKey string) bool {
	dir, err := LeaseDirFor(sessionKey)
	if err != nil {
		return false
	}
	_, rerr := lease.ReadRecord(dir)
	return rerr == nil
}

// SessionSpec is one launch's already-decided facts. Every field is resolved
// by the command layer BEFORE the lifecycle lock is taken; nothing here is
// re-derived under it except the runtime state itself (see RunSession).
type SessionSpec struct {
	Key       string // lease identity (SessionName)
	Name      string // the ACTUAL sbx sandbox name this launch targets
	Workspace string // canonical workspace, for the MCP receipt

	// Creating is DefinitelyCreating: this launch is CERTAIN to create a
	// fresh sandbox, so the create-evidence poll, the MCP receipt and the
	// lease record all apply. Anything less certain attaches.
	Creating bool
	// Keep is -k/--keep: bind an identity-bound keep AFTER the record exists.
	Keep bool

	// CreateArgs is the composed `sbx ...` argv for this launch as planned:
	// the create argv when Creating, and the legacy `sbx run --name` re-attach
	// argv otherwise. An attach to a POSITIVELY IDENTIFIED RUNNING sandbox
	// replaces it with an `sbx exec` argv built under the lock (see
	// RunSession); a stopped or schema-unverified sandbox keeps it, because
	// exec has no "start" of its own.
	CreateArgs []string
	// AttachTTY selects `sbx exec -it` (interactive) vs `-i` (piped).
	AttachTTY bool
	// AttachExec is true when the pre-lock probe positively identified a
	// RUNNING, schema-verified sandbox — the only case in which an attach may
	// exec into it directly.
	AttachExec bool

	Fingerprint sandbox.Fingerprint // recorded on create, validated on attach
	Invocation  []string            // the exact pi argv this create sends
	// DefaultInvocation is the "safe current/default" pi argv an attach uses
	// when nothing was ever recorded (a legacy or never-owned sandbox) — the
	// same BuildPiInvocation a create would have sent, never an invented one.
	DefaultInvocation []string
	Preloaded         []string // the exact --static-mcp set, for the receipt
}

// SessionDeps are the collaborators RunSession cannot conjure: the env its
// probes run through, the create-evidence poll, and the warn stream it
// reports best-effort degradations on. Passed in, never defaulted — an
// unwired poll is an error, not a silent "record nothing" (see
// CreatePoll.validate).
type SessionDeps struct {
	Env  hostenv.Env
	Poll CreatePoll
	Warn io.Writer
	// Spawn builds the child process for argv. The command layer owns stdio
	// and environment wiring; this package owns only WHEN it starts and what
	// argv it gets.
	Spawn func(argv []string) *exec.Cmd
	// LockTimeout bounds the lifecycle-lock acquire; zero means
	// SessionLockTimeout.
	LockTimeout time.Duration
	// Teardown tunes the LAST-SHELL teardown attempt this session makes after
	// its child exits (see reap.go). The zero value is the production shape;
	// a test tightens the bounds or redirects the journal through it.
	Teardown TeardownOptions
}

// SessionRefused is a lifecycle refusal decided UNDER the lifecycle lock: the
// child was never started, and nothing was created, removed, or recorded. It
// is a distinct type so the command layer can print it as run's own complete
// message (exit 1) instead of dressing it up as an exec failure.
type SessionRefused struct{ Err error }

func (e *SessionRefused) Error() string { return e.Err.Error() }
func (e *SessionRefused) Unwrap() error { return e.Err }

// RunSession is the whole create/attach lifecycle for one session, in the one
// order that is safe (see this file's header):
//
//  1. lifecycle lock EXCLUSIVE, deadline-bounded;
//  2. RE-PROBE the runtime under it — the pre-lock decision is advisory, the
//     state under the lock is the truth. A create whose sandbox now exists,
//     or an attach whose sandbox has vanished, is REFUSED (SessionRefused):
//     another process won the race, and continuing with argv built for the
//     other answer is exactly the guess this package refuses to make. It
//     never force-removes anything to resolve the race;
//  3. on create: start the child, poll until the sandbox is VISIBLE, then
//     record the immutable instance id, the fingerprint, the exact pi
//     invocation and the MCP receipt. On attach: validate the recorded
//     fingerprint (a divergence refuses; no record attaches UNOWNED with the
//     safe default invocation) and start `sbx exec -it/-i` with the STORED
//     invocation;
//  4. take the refs SHARED lock while STILL holding lifecycle EX
//     (lease.AttachRefUnderLifecycle), so no other process can ever observe
//     this transition as finished with zero holders;
//  5. release lifecycle, THEN wait for the session to exit, holding only the
//     shared reference — which the kernel releases even on SIGKILL.
//
// The returned error is the child's own (an *exec.ExitError the caller maps
// to an exit code), a *workspace.ReceiptRecordError when only local recording
// failed, or a *SessionRefused decided before anything started.
func RunSession(spec SessionSpec, deps SessionDeps) error {
	if deps.Spawn == nil {
		return fmt.Errorf("launch: RunSession needs a Spawn (the command layer owns stdio wiring)")
	}
	dir, derr := LeaseDirFor(spec.Key)
	if derr != nil {
		// An unleasable session still launches: refusing an interactive launch
		// because a state directory could not be created would be a worse
		// failure than launching unserialized. It is loud, because an unleased
		// session is invisible to a future reaper's zero-holder proof.
		fmt.Fprintf(deps.Warn, "pix: warning: running without %s's lifecycle lease (%v); a future orphan sweep could not see this session\n", spec.Key, derr)
		return runSessionUnleased(spec, deps)
	}

	timeout := deps.LockTimeout
	if timeout <= 0 {
		timeout = SessionLockTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var child *SessionChild
	ref, err := lease.AttachRefUnderLifecycle(ctx, dir, func() error {
		var terr error
		child, terr = startSessionTransition(spec, deps)
		return terr
	})
	if err != nil {
		if child != nil {
			// The transition failed AFTER the child started (only the refs
			// acquire can do that): the session is live and must still be
			// waited out, unreferenced but honest about it.
			fmt.Fprintf(deps.Warn, "pix: warning: %s started without a reference lease (%v)\n", spec.Key, err)
			return child.Wait()
		}
		return err
	}
	// Lifecycle released by AttachRefUnderLifecycle; only the SHARED reference
	// is still held, and it is held for exactly as long as the session lives.
	werr := child.Wait()

	// U04d: this shell is out. Drop OUR reference FIRST and only then attempt
	// the reaper's proof — the order is the whole mechanism, because
	// TryReapProof's refs EXCLUSIVE can never succeed while this process still
	// holds its own SHARED reference, so a teardown attempted before this Close
	// would report "busy" against itself, every time. A close that fails leaves
	// the reference held (the kernel drops it at exit anyway) and skips the
	// attempt rather than reaping without a valid proof.
	if cerr := ref.Close(); cerr != nil {
		fmt.Fprintf(deps.Warn, "pix: warning: releasing %s's reference lease: %v; skipping teardown\n", spec.Key, cerr)
		return werr
	}
	reportTeardown(deps.Warn, TeardownSandbox(deps.Env, spec.Key, spec.Name, TriggerSession, deps.Teardown))
	return werr
}

// reportTeardown prints the one line a last-shell teardown is allowed to say,
// on the caller's warn stream (stderr) — never stdout, which belonged to the
// session. TeardownKeptUnowned is silent on purpose: it is the ordinary
// outcome for a legacy or never-recorded sandbox, and a note on every such exit
// would be noise that trains people to ignore the ones that matter.
func reportTeardown(warn io.Writer, res TeardownResult) {
	switch res.Verdict {
	case TeardownKeptUnowned:
		return
	case TeardownRemoved, TeardownAlreadyAbsent:
		fmt.Fprintf(warn, "pix: %s\n", res.Detail)
	default:
		fmt.Fprintf(warn, "pix: kept %s: %s\n", res.Sandbox, res.Detail)
	}
}

// runSessionUnleased is the degraded path for a host whose lease state dir is
// unusable: the same create/attach steps, no locking and no recording. Kept
// as its own function so the leased path above reads as the one true ordering
// rather than a maze of nil checks.
func runSessionUnleased(spec SessionSpec, deps SessionDeps) error {
	child, err := StartSbxRunAndRecordCreate(deps.Spawn(spec.CreateArgs), deps.Poll, spec.Creating, spec.Name, spec.Workspace, spec.Preloaded)
	if err != nil {
		return err
	}
	return child.Wait()
}

// startSessionTransition is the body that runs UNDER the lifecycle lock. It
// returns as soon as the transition is recorded, with the child still
// running: waiting for a session is not a lifecycle transition (see
// lease.AttachRefUnderLifecycle's doc on why fn must be brief).
func startSessionTransition(spec SessionSpec, deps SessionDeps) (*SessionChild, error) {
	argv := spec.CreateArgs
	if spec.Creating {
		// A create that finds the sandbox already there lost the race: another
		// process created it while this one was resolving kits. Refuse — the
		// argv in hand creates, and "create over an existing sandbox" is
		// undefined (sbx may error, or silently attach with stale flags).
		if state := ProbeTaskSandbox(deps.Env, spec.Name); sandboxAppeared(state) {
			return nil, &SessionRefused{Err: fmt.Errorf("%q appeared while this launch was preparing (another `pix run` won the race) — nothing was created or removed; re-run to attach to it", spec.Name)}
		}
	} else {
		// An attach whose sandbox is gone must not fall through to a create
		// path it never resolved create-only flags for.
		if state := ProbeTaskSandbox(deps.Env, spec.Name); state == SbxAbsent {
			return nil, &SessionRefused{Err: fmt.Errorf("%q disappeared while this launch was preparing — nothing was created or removed; re-run to create it", spec.Name)}
		}
		if diverged, found := CheckSessionFingerprint(spec.Key, spec.Fingerprint); found && len(diverged) > 0 {
			return nil, &SessionRefused{Err: fmt.Errorf("%q was created with a different %s — refusing to attach. Recreate it explicitly: %s",
				spec.Name, strings.Join(diverged, ", "), RunReplaceCommand(spec.Workspace))}
		}
		if spec.AttachExec {
			// The stored create-time invocation is replayed VERBATIM; a
			// missing or corrupt record attaches UNOWNED with the safe
			// recomputed default rather than refusing — and, having no
			// recorded identity, never authorizes a removal (there is no
			// teardown here at all, and no record for one to consult).
			invocation, hasRecord := ReadSessionInvocation(spec.Key)
			if !hasRecord {
				invocation = spec.DefaultInvocation
			}
			execArgs, aerr := BuildAttachArgv(spec.Name, spec.AttachTTY, invocation)
			if aerr != nil {
				return nil, &SessionRefused{Err: aerr}
			}
			argv = execArgs
		}
	}

	child, err := StartSbxRunAndRecordCreate(deps.Spawn(argv), deps.Poll, spec.Creating, spec.Name, spec.Workspace, spec.Preloaded)
	if err != nil {
		return nil, err
	}
	// Everything a later attach — or a future reaper — reads is written HERE,
	// before the lifecycle lock is released and before the session is waited
	// for: a creator killed mid-session still leaves a complete record.
	recorded := SessionRecorded(spec.Key)
	if spec.Creating && child.Appeared {
		var rerr error
		recorded, rerr = RecordSessionCreation(deps.Env, spec.Key, spec.Name, spec.Fingerprint, spec.Invocation)
		if rerr != nil {
			fmt.Fprintf(deps.Warn, "pix: warning: %v\n", rerr)
		}
	}
	if spec.Keep {
		switch {
		case !recorded:
			fmt.Fprintf(deps.Warn, "pix: warning: -k/--keep not recorded: %q has no verified creation record to bind a keep to\n", spec.Name)
		default:
			if kerr := SetSessionKeep(spec.Key); kerr != nil {
				fmt.Fprintf(deps.Warn, "pix: warning: -k/--keep could not be recorded: %v\n", kerr)
			}
		}
	}
	return child, nil
}
