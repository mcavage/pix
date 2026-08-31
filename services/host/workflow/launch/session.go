//go:build unix

// session.go — the CREATE/ATTACH half of the sandbox lifecycle, over the two
// packages that own the primitives: pix/host/lease (the immutable creation
// record, the identity-bound keep, the lifecycle.lock/refs.lock pair) and
// pix/host/sandbox (the digest name, the `sbx ls` parse, create-vs-exec argv,
// the fingerprint diff). The ordering it exists to get right, once:
//
//	lifecycle EX  ->  re-probe the runtime under it  ->  start the child
//	              ->  (create) poll until visible, then record instance id,
//	                  fingerprint and invocation
//	              ->  refs SH (still under lifecycle EX)
//	              ->  lifecycle UNLOCK  ->  wait for the session to exit
//
// Three properties are load-bearing, and the process tests prove them:
// everything a later attach or a reaper reads is on disk BEFORE any other
// process can observe the transition as finished (so a killed creator still
// leaves a complete record); the lifecycle lock covers only the TRANSITION,
// never the session's own lifetime; and a child the transition already
// started must never be left running WITHOUT a reference lease — if
// lease.AttachRefUnderLifecycle's refs acquire fails after fn (this file's
// startSessionTransition) already started it, RunSession kills it rather
// than waiting it out unreferenced, because a future reaper's zero-holder
// proof cannot tell "live and unreferenced" apart from "orphaned" — so that
// kill is followed by the SAME proof-gated TeardownSandbox a normal session
// exit uses, rather than leaving the sandbox for a future orphan sweep to
// eventually find. Teardown is next door in reap.go: this file's last acts,
// on every path out of RunSession, are to CLOSE this shell's reference (or
// give up trying to acquire one) and hand the decision to TeardownSandbox.
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

const SessionLockTimeout = 30 * time.Second

func lifecycleIdentity() string {
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

func leaseRoot() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, "sandboxes"), nil
}

func SessionName(workspace string) string { return sandbox.Name(workspace) }

func leaseDirFor(sessionKey string) (string, error) {
	root, err := leaseRoot()
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

// setSessionKeep binds the sticky, identity-bound keep marker -k/--keep asks for:
func setSessionKeep(sessionKey string) error {
	dir, err := leaseDirFor(sessionKey)
	if err != nil {
		return err
	}
	return lease.SetKeep(dir, lifecycleIdentity())
}

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

const (
	sessionFingerprintFileName = "fingerprint.json"
	sessionInvocationFileName  = "invocation.json"
)

func writeSessionState(sessionKey, name string, v any) error {
	dir, err := leaseDirFor(sessionKey)
	if err != nil {
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("launch: marshal %s: %w", name, err)
	}
	if err := writeFileAtomic(filepath.Join(dir, name), data, 0o600); err != nil {
		return fmt.Errorf("launch: write %s: %w", name, err)
	}
	return nil
}

func readSessionState(sessionKey, name string, v any) bool {
	dir, err := leaseDirFor(sessionKey)
	if err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

func CheckSessionFingerprint(sessionKey string, current sandbox.Fingerprint) (diverged []string, found bool) {
	var stored sandbox.Fingerprint
	if !readSessionState(sessionKey, sessionFingerprintFileName, &stored) {
		return nil, false
	}
	return sandbox.Diff(stored, current), true
}

func readSessionFingerprint(sessionKey string) (sandbox.Fingerprint, bool) {
	var fp sandbox.Fingerprint
	if !readSessionState(sessionKey, sessionFingerprintFileName, &fp) || len(fp) == 0 {
		return nil, false
	}
	return fp, true
}

func readSessionInvocation(sessionKey string) (invocation []string, found bool) {
	if !readSessionState(sessionKey, sessionInvocationFileName, &invocation) {
		return nil, false
	}
	return invocation, true
}

// sbxListing is the module's ONE parser of `sbx ls --json` (slim_test.go's
// TestLifecycle_OneParserPerSbxListing pins that), and the ONE bounded read
// every environment-scoped question (live holders, effective-file release,
// the create receipt) is answered from. trusted means the listing itself
// was READ and parsed into a documented shape inside within; per-row
// identity stays the caller's own check (sandbox.Entry.IdentityVerified),
// because one unowned row in a shared listing must not invalidate the
// answer about a different, positively identified sandbox. An untrusted
// answer is never downgraded into "absent" by any caller — that is the
// fail-closed half of each of those questions.
func sbxListing(env hostenv.Env, within time.Duration) (entries []sandbox.Entry, trusted bool) {
	var out string
	var err error
	if within > 0 {
		var timedOut bool
		out, timedOut, err = env.RunWithin(within, "sbx", "ls", "--json")
		if timedOut {
			return nil, false
		}
	} else {
		out, err = env.Run("sbx", "ls", "--json")
	}
	if err != nil {
		return nil, false
	}
	parsed, perr := sandbox.ParseList([]byte(out))
	if perr != nil {
		return nil, false
	}
	return parsed.Entries, true
}

func sbxEntry(env hostenv.Env, name string, within time.Duration) (entry *sandbox.Entry, trusted bool) {
	entries, ok := sbxListing(env, within)
	if !ok {
		return nil, false
	}
	return sandbox.FindByName(entries, name), true
}

// FindPositivelyIdentified reports the schema-verified, RUNNING-OR-STOPPED
// row for name — the identity check a reattach gate needs regardless of
// which of the two live states the sandbox is actually in (a stopped
// sandbox is still a legitimate reattach target, docs/getting-started.md:
// "A sandbox already exists -> reattach, running or stopped, as-is"). An
// absent, unverified, or unreadable row authorizes nothing, exactly like
// FindPositivelyIdentifiedRunning.
func FindPositivelyIdentified(env hostenv.Env, name string) (*sandbox.Entry, bool) {
	found, _ := sbxEntry(env, name, 0)
	if found == nil || !found.IdentityVerified {
		return nil, false
	}
	if found.State != sandbox.StateRunning && found.State != sandbox.StateStopped {
		return nil, false
	}
	return found, true
}

// FindPositivelyIdentifiedRunning is the RUNNING-only predicate a caller
// authorizing `sbx exec` needs (exec has no "start" of its own — it fails
// outright against a stopped sandbox). A stopped, schema-verified row is
// deliberately NOT positively-identified-running here: the caller must fall
// back to the legacy `sbx run --name` reattach path instead, which is what
// actually starts a stopped sandbox (see BuildReattachArgs).
func FindPositivelyIdentifiedRunning(env hostenv.Env, name string) (*sandbox.Entry, bool) {
	found, ok := FindPositivelyIdentified(env, name)
	if !ok || found.State != sandbox.StateRunning {
		return nil, false
	}
	return found, true
}

// PromoteSessionCreation is the cutover's create-receipt step: a bounded,
// schema-verified probe must positively identify the sandbox and its
// instance id, and only then is the bounded create intent PROMOTED into the
// instance-bound, write-once lease record (E2.3's PromoteCreateIntent),
// followed by this session's fingerprint and exact invocation.
//
// The returned warning is the promotion's CleanupWarning: the lease record
// is already durable and only the redundant intent file survived. It is
// reported, never treated as a failed create — the exact false-abort
// PromotionResult was shaped to make unreachable.
func PromoteSessionCreation(env hostenv.Env, sessionKey, name string, fp sandbox.Fingerprint, invocation []string) (recorded bool, warning error, err error) {
	found, _ := sbxEntry(env, name, 0)
	if found == nil || !found.IdentityVerified || found.InstanceID == nil || *found.InstanceID == "" {
		return false, nil, nil // present but unowned: no verifiable instance id to promote against
	}
	dir, derr := leaseDirFor(sessionKey)
	if derr != nil {
		return false, nil, derr
	}
	res, perr := PromoteCreateIntent(dir, CreateReceipt{InstanceID: *found.InstanceID, SandboxName: found.Name})
	if perr != nil {
		return false, nil, fmt.Errorf("launch: could not record %s's creation lease: %w", name, perr)
	}
	if !res.Promoted() {
		return false, nil, fmt.Errorf("launch: %s's creation lease was not recorded", name)
	}
	if werr := writeSessionState(sessionKey, sessionFingerprintFileName, fp); werr != nil {
		return false, res.CleanupWarning, fmt.Errorf("launch: could not record %s's session fingerprint: %w", name, werr)
	}
	if werr := writeSessionState(sessionKey, sessionInvocationFileName, invocation); werr != nil {
		return false, res.CleanupWarning, fmt.Errorf("launch: could not record %s's session invocation: %w", name, werr)
	}
	return true, res.CleanupWarning, nil
}

func RecordSessionCreation(env hostenv.Env, sessionKey, name string, fp sandbox.Fingerprint, invocation []string) (recorded bool, err error) {
	found, _ := sbxEntry(env, name, 0)
	if found == nil || !found.IdentityVerified || found.InstanceID == nil || *found.InstanceID == "" {
		return false, nil // present but unowned: no verifiable instance id to record
	}
	dir, err := leaseDirFor(sessionKey)
	if err != nil {
		return false, err
	}
	if _, err := lease.CreateRecord(dir, *found.InstanceID); err != nil {
		return false, fmt.Errorf("launch: could not record %s's creation lease: %w", name, err)
	}
	if err := writeSessionState(sessionKey, sessionFingerprintFileName, fp); err != nil {
		return false, fmt.Errorf("launch: could not record %s's session fingerprint: %w", name, err)
	}
	if err := writeSessionState(sessionKey, sessionInvocationFileName, invocation); err != nil {
		return false, fmt.Errorf("launch: could not record %s's session invocation: %w", name, err)
	}
	return true, nil
}

func SessionRecorded(sessionKey string) bool {
	dir, err := leaseDirFor(sessionKey)
	if err != nil {
		return false
	}
	_, rerr := lease.ReadRecord(dir)
	return rerr == nil
}

type SessionSpec struct {
	Key  string // lease identity (SessionName)
	Name string // the ACTUAL sbx sandbox name this launch targets

	// Creating is the one create-vs-attach fact, decided once by the command layer
	// from a POSITIVE probe. Anything less certain attaches.
	Creating bool
	// Keep is -k/--keep: bind an identity-bound keep AFTER the record exists.
	Keep bool

	// Workspace is the host dir this session ran in. Carried so teardown can
	// look for a persisted session beside it and say how to resume.
	Workspace string

	// EnvCreateArgs, when non-empty, makes a CREATE two-phase — E2.5's
	// cutover shape (docs/design/environments.md §10.1): `sbx env create
	// <effective>` runs to completion FIRST, the positively identified
	// instance is recorded (lease promoted to that instance id), and only
	// THEN is the session started as a name-based `sbx exec -it <name> --
	// pi <exact invocation>`. There is no second selectable create path:
	// this is the ONLY create shape: an `sbx run`-style create argv is not
	// a field on this type at all after the cutover, so no crash, lease
	// failure or unset flag can select one (PRD §8; envargv_sentinel_test.go).
	EnvCreateArgs []string
	// AttachArgs is the ONE non-exec attach argv a caller may supply (the
	// pre-cutover `run --name` re-attach). It is NEVER used for a create.
	AttachArgs []string
	// AttachTTY selects `sbx exec -it` (interactive) vs `-i` (piped).
	AttachTTY bool
	// AttachExec is true when the pre-lock probe positively identified a RUNNING,
	// schema-verified sandbox — the only case an attach may exec.
	AttachExec bool

	Fingerprint sandbox.Fingerprint // recorded on create, validated on attach
	// Invocation is the exact pi argv a CREATE sends and records for audit
	// (PromoteSessionCreation persists it as sessionInvocationFileName); it
	// is never read back to decide what a LATER attach execs.
	Invocation []string
	// DefaultInvocation is the pi argv THIS session actually execs, on both
	// a create and an attach: the current, freshly resolved invocation this
	// call built (model/resume/skills as requested THIS run), never a
	// replay of a prior create's stored one (QA re-review F1). The command
	// layer sets this to the SAME value as Invocation on every call
	// (run_cmd.go builds one invocation and uses it for both fields), so
	// the two names exist to document the two different questions
	// ("what got recorded" vs "what gets exec'd"), not to carry two
	// different values.
	DefaultInvocation []string
}

// SessionDeps are the collaborators RunSession cannot conjure. Passed in, never
// defaulted — an unwired poll is an error, not a silent "record nothing".
type SessionDeps struct {
	Env  hostenv.Env
	Poll CreatePoll
	Warn io.Writer
	// Spawn builds the child process for argv. The command layer owns stdio and
	// environment wiring; this package owns only WHEN it starts and its argv.
	Spawn func(argv []string) *exec.Cmd
	// LockTimeout bounds the lifecycle-lock acquire; zero means
	// SessionLockTimeout.
	LockTimeout time.Duration
	// Teardown tunes the LAST-SHELL teardown this session attempts after its
	// child exits. The zero value is the production shape.
	Teardown TeardownOptions
}

type SessionRefused struct{ Err error }

func (e *SessionRefused) Error() string { return e.Err.Error() }
func (e *SessionRefused) Unwrap() error { return e.Err }

// RunSession is the whole create/attach lifecycle for one session, in the one
// order that is safe (see this file's header):
func RunSession(spec SessionSpec, deps SessionDeps) error {
	if deps.Spawn == nil {
		return fmt.Errorf("launch: RunSession needs a Spawn (the command layer owns stdio wiring)")
	}
	// A lease/state-dir failure is a HARD refusal, not a degraded launch.
	// Before the cutover this fell back to spawning the old create argv
	// without any lease at all; after it there is no second create path to
	// fall back TO, and "create a sandbox nothing on this host can prove it
	// owns" is precisely what the create-intent/receipt machinery exists to
	// prevent (PRD §8, §9.3).
	dir, derr := leaseDirFor(spec.Key)
	if derr != nil {
		return &SessionRefused{Err: fmt.Errorf("cannot take %s's lifecycle lease (%v); refusing to create or attach without one", spec.Key, derr)}
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
			killUnreferencedAndTeardown(spec, deps, child, err)
		}
		return err
	}
	// Lifecycle released by AttachRefUnderLifecycle; only the SHARED reference
	// is still held, and it is held for exactly as long as the session lives.
	werr := child.Wait()

	if cerr := ref.Close(); cerr != nil {
		fmt.Fprintf(deps.Warn, "pix: warning: releasing %s's reference lease: %v; skipping teardown\n", spec.Key, cerr)
		return werr
	}
	res := TeardownSandbox(deps.Env, spec.Key, spec.Name, TriggerSession, deps.Teardown)
	reportTeardown(deps.Warn, res, spec.Workspace)
	releaseEffectiveAfterTeardown(deps, spec.Name, res)
	return werr
}

// releaseEffectiveAfterTeardown clears the retained effective document only
// after a removal, and then only on a POSITIVE absent probe (§10.3). Every
// other outcome — kept, failed, or a listing that could not be read —
// retains the file, because it is what a later environment-scoped removal
// recomposes and deleting it early strands that path on the name-based
// fallback.
func releaseEffectiveAfterTeardown(deps SessionDeps, name string, res TeardownResult) {
	if !res.Removed() {
		return
	}
	if _, err := ReleaseEffectiveEnv(deps.Env, name); err != nil && deps.Warn != nil {
		fmt.Fprintf(deps.Warn, "pix: warning: %v\n", err)
	}
}

// killUnreferencedAndTeardown is RunSession's failed-ref path: the transition
// itself succeeded (fn returned nil, child is live) but the refs acquire that
// runs AFTER it could not — that child is now live with NO reference lease,
// and a future reaper's zero-holder proof cannot distinguish it from an
// orphan. It must never be left running unreferenced, so kill it rather than
// waiting it out, then hand it to the same proof-gated TeardownSandbox a
// normal session exit uses.
//
// child.Kill already blocks until the child's exit is drained off its wait
// channel, but the explicit child.Wait below makes that reap-before-teardown
// ordering a property of THIS function's own code, not merely of Kill's
// current internals: TeardownSandbox's removal asks the sbx runtime to tear
// down the sandbox this process started, and doing that while the local `sbx`
// invocation is still, from the kernel's point of view, dying rather than
// fully reaped is the remove-vs-dying-sbx race — killing the local process
// only ends THIS shell's view of the session, it says nothing on its own
// about whether the runtime has caught up, so the safe order is: kill, then
// WAIT for the reap to be certain, then ask the runtime to remove it.
func killUnreferencedAndTeardown(spec SessionSpec, deps SessionDeps, child *SessionChild, cause error) {
	fmt.Fprintf(deps.Warn, "pix: warning: %s started but could not be given a reference lease (%v); killing it rather than leaving it live and unreferenced\n", spec.Key, cause)
	if kerr := child.Kill(); kerr != nil {
		fmt.Fprintf(deps.Warn, "pix: warning: killing %s after the failed reference lease: %v\n", spec.Key, kerr)
	}
	// Kill has already signalled the child; Wait replays its drained exit
	// (a no-op over Kill's own result) but is the explicit proof, right here,
	// that the process is fully reaped before the next line ever runs.
	_ = child.Wait()
	// Killing the local `sbx` CLI invocation ends THIS shell's view of the
	// session, but the sandbox it started is still live on the sbx runtime and
	// now has no reference lease at all — left alone, it would sit
	// unreferenced until some future orphan sweep happened to run. Hand it to
	// the same proof-gated TeardownSandbox a normal session exit uses, right
	// now that its creator is confirmed gone, and report exactly what it
	// decided.
	reportTeardown(deps.Warn, TeardownSandbox(deps.Env, spec.Key, spec.Name, TriggerSession, deps.Teardown), spec.Workspace)
}

// reportTeardown reports only exceptional last-shell outcomes on the caller's
// warn stream. pi prints the exact host-runnable resume command before exiting,
// and routine automatic removal is expected, so adding a removal confirmation
// here would turn one useful line into three noisy ones.
func reportTeardown(warn io.Writer, res TeardownResult, _ string) {
	switch res.Verdict {
	case TeardownKeptUnowned, TeardownRemoved, TeardownAlreadyAbsent:
		return
	default:
		fmt.Fprintf(warn, "pix: kept %s: %s\n", res.Sandbox, res.Detail)
	}
}

// startEnvCreateTransition is the E2.5 create: `sbx env create <effective>`
// is a CREATE, not a session — it returns as soon as the sandbox exists —
// so it is run to completion under the lifecycle lock, its receipt is
// demanded POSITIVELY (a bounded poll that must see a schema-verified row
// whose instance id is recordable), the lease/fingerprint/invocation are
// promoted against that instance, and only then is the actual session
// started as `sbx exec -it <name> -- pi <exact invocation>`. A create that
// fails, or that leaves nothing positively identifiable behind, refuses
// here: it never falls through to an exec against a sandbox this host
// cannot prove it made.
func startEnvCreateTransition(spec SessionSpec, deps SessionDeps) (*SessionChild, error) {
	creator, err := StartSbxSession(deps.Spawn(spec.EnvCreateArgs), deps.Poll, true, spec.Name)
	if err != nil {
		return nil, err
	}
	if werr := creator.Wait(); werr != nil {
		return nil, werr
	}
	if !creator.Appeared && !settleAfterExit(deps.Poll, spec.Name) {
		return nil, &SessionRefused{Err: fmt.Errorf("`sbx env create` exited successfully but %q never appeared in sbx ls — nothing was attached to", spec.Name)}
	}
	// PROMOTE, don't merely record: the bounded create intent written before
	// the spawn is replaced by the instance-bound lease record (E2.3), which
	// is the ONE thing that ever counts as a positive create receipt. A
	// cleanup warning means the record IS durable and only the now-redundant
	// intent file survived — informational, never a reason to abort a create
	// that already succeeded (PromotionResult's own contract).
	recorded, warning, rerr := PromoteSessionCreation(deps.Env, spec.Key, spec.Name, spec.Fingerprint, spec.Invocation)
	if rerr != nil {
		return nil, rerr
	}
	if warning != nil {
		fmt.Fprintf(deps.Warn, "pix: warning: %v\n", warning)
	}
	if !recorded {
		return nil, &SessionRefused{Err: fmt.Errorf("%q was created but reports no verifiable instance id — refusing to attach to a sandbox this host cannot prove it owns", spec.Name)}
	}
	if spec.Keep {
		if kerr := setSessionKeep(spec.Key); kerr != nil {
			fmt.Fprintf(deps.Warn, "pix: warning: -k/--keep could not be recorded: %v\n", kerr)
		}
	}
	execArgs, aerr := BuildAttachArgv(spec.Name, spec.AttachTTY, spec.Invocation)
	if aerr != nil {
		return nil, &SessionRefused{Err: aerr}
	}
	// The session itself is an ATTACH to a sandbox that already exists, so it
	// is started with creating=false: the create receipt was already demanded
	// above, and re-polling for it here would only re-answer a settled question.
	return StartSbxSession(deps.Spawn(execArgs), deps.Poll, false, spec.Name)
}

// startSessionTransition is the body that runs UNDER the lifecycle lock. It
// returns as soon as the transition is recorded, with the child still running:
func startSessionTransition(spec SessionSpec, deps SessionDeps) (*SessionChild, error) {
	argv := spec.AttachArgs
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
			return nil, &SessionRefused{Err: fmt.Errorf("%q was created with a different %s — refusing to attach. %s",
				spec.Name, strings.Join(diverged, ", "), RecreateGuidance(spec.Name))}
		}
		if spec.AttachExec {
			// QA re-review F1: an attach must use THIS launch's own current,
			// freshly resolved invocation (spec.DefaultInvocation — the exact
			// pi argv BuildPiInvocation just composed from --model/--resume/
			// whatever was requested for THIS run), never a replay of
			// whatever a prior create happened to store. The stored
			// invocation (readSessionInvocation) remains an AUDIT trail only
			// — reap.go still reads it to confirm a sandbox has a verifiable
			// creation record — and is never again consulted to decide what
			// an attach actually execs. Verified live: a re-attach with a new
			// --model/--resume must carry it into the exec argv, not replay
			// the create-time one (docs/design/pix-v2-surface.md §3.1: "both
			// options apply to every attach").
			execArgs, aerr := BuildAttachArgv(spec.Name, spec.AttachTTY, spec.DefaultInvocation)
			if aerr != nil {
				return nil, &SessionRefused{Err: aerr}
			}
			argv = execArgs
		}
	}

	if spec.Creating {
		// THE create path, and the only one: `sbx env create <effective>`.
		if len(spec.EnvCreateArgs) == 0 {
			return nil, &SessionRefused{Err: fmt.Errorf("%q has no effective environment to create from; refusing to create a sandbox outside the environment path", spec.Name)}
		}
		return startEnvCreateTransition(spec, deps)
	}
	if len(argv) == 0 {
		return nil, &SessionRefused{Err: fmt.Errorf("%q has no attach argv; refusing to spawn sbx with nothing to do", spec.Name)}
	}

	child, err := StartSbxSession(deps.Spawn(argv), deps.Poll, spec.Creating, spec.Name)
	if err != nil {
		return nil, err
	}
	// An attach records nothing new: the create-time facts are already on
	// disk (a create wrote them under this same lock before the lock was
	// released). Only the keep marker, which is this shell's own request,
	// is bound here.
	if spec.Keep {
		switch {
		case !SessionRecorded(spec.Key):
			fmt.Fprintf(deps.Warn, "pix: warning: -k/--keep not recorded: %q has no verified creation record to bind a keep to\n", spec.Name)
		default:
			if kerr := setSessionKeep(spec.Key); kerr != nil {
				fmt.Fprintf(deps.Warn, "pix: warning: -k/--keep could not be recorded: %v\n", kerr)
			}
		}
	}
	return child, nil
}
