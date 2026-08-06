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
// proof cannot tell "live and unreferenced" apart from "orphaned". Teardown
// is next door in reap.go: this file's last act is to CLOSE this shell's
// reference and hand the decision to TeardownSandbox.
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

func sbxEntry(env hostenv.Env, name string, within time.Duration) (entry *sandbox.Entry, trusted bool) {
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
	return sandbox.FindByName(parsed.Entries, name), true
}

func FindPositivelyIdentifiedRunning(env hostenv.Env, name string) (*sandbox.Entry, bool) {
	found, _ := sbxEntry(env, name, 0)
	if found == nil || !found.IdentityVerified || found.State != sandbox.StateRunning {
		return nil, false
	}
	return found, true
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

	CreateArgs []string
	// AttachTTY selects `sbx exec -it` (interactive) vs `-i` (piped).
	AttachTTY bool
	// AttachExec is true when the pre-lock probe positively identified a RUNNING,
	// schema-verified sandbox — the only case an attach may exec.
	AttachExec bool

	Fingerprint sandbox.Fingerprint // recorded on create, validated on attach
	Invocation  []string            // the exact pi argv this create sends
	// DefaultInvocation is the "safe default" pi argv an attach uses when nothing
	// was ever recorded — the same BuildPiInvocation a create would have sent.
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
	dir, derr := leaseDirFor(spec.Key)
	if derr != nil {
		fmt.Fprintf(deps.Warn, "pix: warning: running without %s's lifecycle lease (%v); a future orphan sweep could not see this session\n", spec.Key, derr)
		child, err := StartSbxSession(deps.Spawn(spec.CreateArgs), deps.Poll, spec.Creating, spec.Name)
		if err != nil {
			return err
		}
		return child.Wait()
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
			// The transition itself succeeded (fn returned nil): only the
			// refs acquire that runs AFTER it failed. That child is now live
			// with NO reference lease — a future reaper's zero-holder proof
			// cannot distinguish it from an orphan. It must never be left
			// running unreferenced, so kill it rather than waiting it out.
			fmt.Fprintf(deps.Warn, "pix: warning: %s started but could not be given a reference lease (%v); killing it rather than leaving it live and unreferenced\n", spec.Key, err)
			if kerr := child.Kill(); kerr != nil {
				fmt.Fprintf(deps.Warn, "pix: warning: killing %s after the failed reference lease: %v\n", spec.Key, kerr)
			}
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
	reportTeardown(deps.Warn, TeardownSandbox(deps.Env, spec.Key, spec.Name, TriggerSession, deps.Teardown))
	return werr
}

// reportTeardown prints the one line a last-shell teardown may say, on the
// caller's warn stream — never stdout, which belonged to the session.
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

// startSessionTransition is the body that runs UNDER the lifecycle lock. It
// returns as soon as the transition is recorded, with the child still running:
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
			return nil, &SessionRefused{Err: fmt.Errorf("%q was created with a different %s — refusing to attach. %s",
				spec.Name, strings.Join(diverged, ", "), RecreateGuidance(spec.Name))}
		}
		if spec.AttachExec {
			invocation, hasRecord := readSessionInvocation(spec.Key)
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

	child, err := StartSbxSession(deps.Spawn(argv), deps.Poll, spec.Creating, spec.Name)
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
			if kerr := setSessionKeep(spec.Key); kerr != nil {
				fmt.Fprintf(deps.Warn, "pix: warning: -k/--keep could not be recorded: %v\n", kerr)
			}
		}
	}
	return child, nil
}
