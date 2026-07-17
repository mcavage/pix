package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"pi-stack/host/config"
)

const taskUsage = `usage: pi-stack task <new|ls|rm> [args]

Run parallel tasks on one repo. Each task is a local clone with its own branch
and sandbox, so tasks never collide. Commits land in the task's clone on this
host and can be pushed or fetched back.

  new <name> [--from REF] [-- pi-args]   clone + branch + launch a sandbox
  ls  [--json]                           tasks, their branch, sandbox + git state
  rm  <name> [--force]                   tear down sandbox + clone (guarded)

Clones live under $XDG_STATE_HOME/pi-stack/tasks/<repo>/co/<name>, outside your
repo. ` + "`pi-stack task rm`" + ` refuses to drop uncommitted or unpushed work.
`

// taskLaunch is the seam `task new` uses to hand a resolved clone to the run
// path. It is a package var so tests can stub the launch (a real launch needs
// sbx + Docker). The default reuses run.go's helpers without touching run.go.
var taskLaunch = launchTask

// runTask is the verbatim dispatcher for the `task` grouping noun (mirrors
// runState). A bare noun and -h/--help print the group usage and exit 0; an
// unknown subcommand is a usage error (exit 2).
func runTask(argv []string) {
	if len(argv) == 0 || argv[0] == "-h" || argv[0] == "--help" {
		fmt.Print(taskUsage)
		return
	}
	switch argv[0] {
	case "new":
		runTaskNew(defaultShellEnv(), argv[1:])
	case "ls", "list":
		runTaskLs(defaultShellEnv(), argv[1:])
	case "rm", "remove":
		runTaskRm(defaultShellEnv(), argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pi-stack task: unknown subcommand %q\n\n%s", argv[0], taskUsage)
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers (name / path / key). No filesystem writes, no exec.
// ---------------------------------------------------------------------------

// maxTaskNameLen caps the sanitized <name> segment so the full sandbox name
// (pi-stack-t-<8>-<name>[-<profile>]) stays within sbx's bound. An overflow is
// truncated and tagged with a short hash of the full name so it stays unique.
const maxTaskNameLen = 40

// taskRepoKey is the stable per-repo key: the first 8 hex of sha256 of the
// canonical absolute path of the main worktree. Repo-qualifying the sandbox
// name this way means two repos with a task called "fix" never collide.
func taskRepoKey(mainroot string) string {
	p := mainroot
	if abs, err := filepath.Abs(mainroot); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	sum := sha256.Sum256([]byte(p))
	return hex.EncodeToString(sum[:])[:8]
}

// taskStateRoot is the base dir for all task state:
// $XDG_STATE_HOME/pi-stack/tasks (default ~/.local/state/pi-stack/tasks).
func taskStateRoot() string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" {
		return filepath.Join(x, "pi-stack", "tasks")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "state", "pi-stack", "tasks")
}

// taskPaths resolves the checkout dir and metadata file for a task. name is
// expected to be already sanitized by the caller.
func taskPaths(repokey, name string) (co, meta string) {
	base := filepath.Join(taskStateRoot(), repokey)
	co = filepath.Join(base, "co", name)
	meta = filepath.Join(base, "meta", name+".json")
	return co, meta
}

// reserveTaskCheckout atomically claims the checkout dir co. It creates co's
// parent tree, then os.Mkdir(co): mkdir fails with fs.ErrExist when co already
// exists, which is the atomic "task exists" signal (no check-then-create race
// between concurrent `task new` for the same name). It returns owned=true only
// when THIS call created co, so callers can gate rollback (os.RemoveAll) on
// ownership and never delete a directory a sibling invocation is using.
func reserveTaskCheckout(co string) (owned bool, err error) {
	if err := os.MkdirAll(filepath.Dir(co), 0o700); err != nil {
		return false, err
	}
	if err := os.Mkdir(co, 0o700); err != nil {
		return false, err
	}
	return true, nil
}

// taskLockPath is the per-task advisory lock file:
// $STATE/pi-stack/tasks/<repokey>/locks/<sanitizedname>.lock. It sits OUTSIDE
// the co/ and meta/ trees `task rm` deletes, so the lock file itself is never a
// removed artifact (it is tiny and harmless to leave behind).
func taskLockPath(repokey, name string) string {
	return filepath.Join(taskStateRoot(), repokey, "locks", sanitizeTaskName(name)+".lock")
}

// withTaskLock serializes EVERY lifecycle operation for a single (repokey, name)
// behind an exclusive advisory file lock, so `task new` and `task rm` on the
// same name are mutually exclusive (new-vs-new, rm-vs-rm, and new-vs-rm). This
// is what closes the same-name concurrency class: instead of plugging each
// microsecond window (a concurrent `task rm` deleting a concurrent `task new`'s
// fresh meta, two `task new` racing the reservation), the whole critical section
// runs under one lock. It ensures the locks dir exists (0700), opens/creates the
// lock file, takes syscall.Flock(LOCK_EX), runs fn, then ALWAYS releases
// (LOCK_UN) and closes on every path via defer. A blocking exclusive lock is the
// safe default: lifecycle operations are short. If the lock cannot be acquired
// or opened it returns a clear error and fn never runs unlocked.
//
// Callers must NOT call os.Exit inside fn — os.Exit skips defers, so the lock
// would never release. The pattern is to return an exit code out through a
// captured variable and os.Exit only AFTER withTaskLock returns.
func withTaskLock(repokey, name string, fn func() error) error {
	lockPath := taskLockPath(repokey, name)
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return fmt.Errorf("create task lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open task lock %s: %w", lockPath, err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("acquire task lock %s: %w", lockPath, err)
	}
	// LIFO defers: unlock BEFORE close (closing the fd would also drop the lock,
	// but the explicit LOCK_UN keeps the release intent obvious).
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

// sanitizeTaskName keeps a task name safe as a path + sandbox-name segment
// (reuses the profile sanitizer's shape) and caps its length, hashing the tail
// when it would overflow.
func sanitizeTaskName(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "task"
	}
	if len(out) > maxTaskNameLen {
		sum := sha256.Sum256([]byte(out))
		out = out[:maxTaskNameLen-7] + "-" + hex.EncodeToString(sum[:])[:6]
	}
	return out
}

// taskSandboxName is the collision-proof sandbox name for a task:
// "pi-stack-t-" + repokey + "-" + sanitize(name) [+ "-" + profile]. The profile
// suffix is added only for a NAMED (non-default) profile, mirroring run.go.
func taskSandboxName(repokey, name, profile string) string {
	n := "pi-stack-t-" + repokey + "-" + sanitizeTaskName(name)
	if profile != "" && profile != config.DefaultProfile {
		n += "-" + sanitizeProfileName(profile)
	}
	return n
}

// ---------------------------------------------------------------------------
// Metadata (§3): the single source of truth ls/rm read.
// ---------------------------------------------------------------------------

type taskMeta struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"`
	Sandbox  string `json:"sandbox"`
	Mainroot string `json:"mainroot"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	Origin   string `json:"origin"`
	Profile  string `json:"profile"`
	Created  string `json:"created"`
}

func writeTaskMeta(path string, m taskMeta) error {
	// 0700 dir + 0600 file: meta.json can carry a tokenized origin URL, so it
	// must never be world-readable.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Strip any embedded credentials (user:pass@) from the origin before it hits
	// disk; the live remote URL used for `git push` keeps them, this copy does not.
	m.Origin = stripURLUserinfo(m.Origin)
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// stripURLUserinfo removes any user:pass@ userinfo from a URL, keeping only
// scheme, host, and path. A value that does not parse as a scheme+host URL
// (e.g. an scp-style `git@host:path` remote, which carries no inline token) is
// returned unchanged.
func stripURLUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

// hardenTaskMeta re-derives the security-sensitive fields (mainroot, branch,
// sandbox) from the validated task name so a tampered meta.json can never steer
// a git refspec or `sbx rm` argv against the main repo. fileBase is the file's
// name without the .json suffix; the stored name MUST sanitize back to it,
// otherwise the file was renamed or hand-edited and we refuse to trust it.
//
// The sandbox name is derived from the profile STORED at create time (validated
// + sanitized here, so the M2 tamper guarantee still holds), NOT the currently
// active profile: a task created under `work` must keep targeting its `-work`
// sandbox even after the host switches back to `default`, or rm/ls would point
// at a non-existent sandbox. The Profile field is REQUIRED: falling back to the
// current profile would mis-target the sandbox (rm could tear down a clone while
// leaving the real sandbox live), so a meta without it is treated as invalid.
func hardenTaskMeta(m taskMeta, mainroot, repokey, fileBase string) (taskMeta, error) {
	sane := sanitizeTaskName(m.Name)
	if sane != fileBase {
		return m, fmt.Errorf("metadata name %q does not match its file %q.json", m.Name, fileBase)
	}
	if strings.TrimSpace(m.Profile) == "" {
		return m, fmt.Errorf("metadata for %q has no profile; cannot determine which sandbox it owns", m.Name)
	}
	m.Mainroot = mainroot
	m.Branch = "pi-stack/" + sane
	m.Sandbox = taskSandboxName(repokey, m.Name, sanitizeProfileName(m.Profile))
	return m, nil
}

func readTaskMeta(path string) (taskMeta, error) {
	var m taskMeta
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// ---------------------------------------------------------------------------
// Teardown guard (§4): pure decision, gathered facts feed it.
// ---------------------------------------------------------------------------

// taskState carries the facts the removal guard decides on. The sandbox tri-state
// is the SINGLE source of truth for the sandbox's liveness — gatherTaskState
// probes once and stores it here, so the guard and the teardown executor never
// re-derive running-ness from independent probes (the R3-1 fail-open race).
type taskState struct {
	sandbox  sbxState // the task's sandbox tri-state (running/stopped/absent/unknown)
	dirty    bool     // the clone has uncommitted changes
	unrec    int      // commits reachable from ANY clone head/HEAD that live ONLY here: not in the main repo AND not on a clone remote (the full would-lose-work count)
	unpushed int      // commits ahead of the task branch's upstream (display only), or unrec when there is no upstream
	unknown  bool     // the unrec probe failed; treat as possibly-unrecoverable (fail-safe)
}

// taskRemoveGuard is the pure removal decision. It refuses (ok=false) with a
// teachable reason when the sandbox is still running OR its state cannot be
// determined (fail-safe on the tri-state probe), the clone is dirty, or it
// holds ANY commit that lives ONLY here and is not on a remote — the precise
// "would-lose-work" condition, which unrec now measures across every clone
// head + HEAD (scratch branches and a detached HEAD included), not just the
// task branch. force overrides every refusal. The caller adds the exact
// recovery commands (which need paths the guard does not carry).
func taskRemoveGuard(state taskState, force bool) (blockMsg string, ok bool) {
	if force {
		return "", true
	}
	var reasons []string
	switch state.sandbox {
	case sbxRunning:
		reasons = append(reasons, "the sandbox is still running; stop the session first (sbx stop <name>)")
	case sbxUnknown:
		reasons = append(reasons, "cannot determine sandbox state (sbx ls failed); resolve, then retry")
	}
	if state.dirty {
		reasons = append(reasons, "the clone has uncommitted changes")
	}
	if state.unknown {
		reasons = append(reasons, "could not determine whether this clone holds unrecovered commits (git probe failed)")
	}
	if state.unrec > 0 {
		reasons = append(reasons, fmt.Sprintf("%d commit(s) exist only in this clone and are not pushed", state.unrec))
	}
	if len(reasons) == 0 {
		return "", true
	}
	return strings.Join(reasons, "; "), false
}

// taskForceSnapshotAbort is the pure L1 decision: even a --force removal must
// abort (leaving the clone intact) when the recovery snapshot failed AND the
// work might be unrecoverable. Fail-safe: "might be" covers both a known count
// of clone-only commits (unrec > 0) AND an UNKNOWN state (the unrec probe
// failed), so a probe we could not run never green-lights a destructive drop.
func taskForceSnapshotAbort(force bool, unrec int, unknown, snapshotFailed bool) bool {
	return force && snapshotFailed && (unrec > 0 || unknown)
}

// taskTeardownAbort is the fresh-probe fail-safe decision, made AFTER the guard
// passed but BEFORE any `sbx rm -f`. A final sandbox probe that reads running or
// unknown must still refuse to proceed unless --force is set. This closes the
// teardown race (R3-1): the gather-time state may have said stopped/absent, but
// the sandbox has since come up (or sbx can no longer be reached), and reaching
// `sbx rm -f` on it without --force would kill a live session and delete its
// clone. Under --force the user has accepted stopping/removing, so it proceeds.
func taskTeardownAbort(final sbxState, force bool) bool {
	if force {
		return false
	}
	return final == sbxRunning || final == sbxUnknown
}

// executeTaskTeardown runs the guarded teardown AFTER taskRemoveGuard has passed,
// using the SAME injectable shellEnv the guard's facts came from. Order: snapshot
// the branch to refs/pi-stack/recovered/<name>, re-probe the sandbox for a friendly
// early message, then ALWAYS attempt the atomic `sbx rm`. The removal is where
// correctness lives: non-force issues `sbx rm` (no -f) on EVERY call — even when
// the fresh probe read absent — because the probe-then-delete gap is a TOCTOU (a
// sandbox can be created after the probe), so sbx's own exit status, not the
// probe, is the atomic authority (R6-1). sbx refuses a running sandbox without -f;
// on any rm failure the executor re-probes once and only proceeds when the
// sandbox is now absent (the rm failed because it was already gone). --force
// issues `sbx rm -f`, treating a not-found failure as success. It writes any
// user-facing explanation to w and returns 0 to proceed to artifact removal, or 1
// to abort with everything left intact. st carries the gather-time facts the L1
// snapshot-abort needs. This is the single choke point that guarantees a live or
// indeterminate sandbox is never dropped, and the clone is never deleted without
// first attempting the atomic rm.
func executeTaskTeardown(env shellEnv, w io.Writer, meta taskMeta, co, name, recovered string, force bool, st taskState) int {
	// a. ALWAYS snapshot ALL of the clone's local heads + HEAD to a durable
	// namespace in the main repo, so even a --force teardown is fully recoverable:
	// work on scratch branches and a detached HEAD is captured, not just the task
	// branch. recovered is the namespace prefix (refs/pi-stack/recovered/<name>).
	snapshotArgs := []string{"-C", meta.Mainroot, "fetch", "-q", co,
		"+refs/heads/*:" + recovered + "/heads/*",
		"+HEAD:" + recovered + "/HEAD"}
	if out, err := env.run("git", snapshotArgs...); err != nil {
		fmt.Fprintf(w, "pi-stack task rm: warning: could not snapshot to %s: %v\n%s", recovered, err, out)
		// --force still must not silently drop unrecoverable work: if the snapshot
		// failed AND the work might be unrecoverable (clone-only commits, or an
		// UNKNOWN unrec state), abort and leave the clone intact.
		if taskForceSnapshotAbort(force, st.unrec, st.unknown, true) {
			if st.unknown {
				fmt.Fprintf(w, "pi-stack task rm: refusing to remove %q: the recovery snapshot failed and this clone's unrecovered-commit state is unknown (git probe failed).\n", name)
			} else {
				fmt.Fprintf(w, "pi-stack task rm: refusing to remove %q: the recovery snapshot failed and %d commit(s) live only in this clone.\n", name, st.unrec)
			}
			fmt.Fprintln(w, "Fetch or push that work manually before removing:")
			fmt.Fprintf(w, "  git -C %s fetch %s %s\n", meta.Mainroot, co, meta.Branch)
			if meta.Origin != "" {
				fmt.Fprintf(w, "  git -C %s push origin %s\n", co, meta.Branch)
			}
			return 1
		}
	}

	// b. Re-probe for TOCTOU freshness and RE-APPLY the guard's refusal on the
	// FRESH state. This is the fail-safe: a running/unknown sandbox never reaches
	// `sbx rm -f` without --force.
	final := probeTaskSandbox(env, meta.Sandbox)
	if taskTeardownAbort(final, force) {
		if final == sbxRunning {
			fmt.Fprintf(w, "pi-stack task rm: refusing to remove %q: the sandbox is now running; stop the session first (sbx stop %s).\n", name, meta.Sandbox)
		} else {
			fmt.Fprintf(w, "pi-stack task rm: refusing to remove %q: cannot determine the state of sandbox %q (sbx ls failed); leaving clone intact at %s.\n", name, meta.Sandbox, co)
			fmt.Fprintln(w, "Resolve (check `sbx ls`), then retry.")
		}
		return 1
	}
	// c. The DESTRUCTIVE decision. The fresh probe above is ONLY an early,
	// friendly message: the probe-then-rm gap is a TOCTOU (a sandbox can be
	// created — mounting this clone — between the probe and the deletion), so a
	// probe reading absent must NOT green-light deleting the checkout. The atomic
	// authority is `sbx rm` itself, always attempted (R6-1). sbx is docker-style:
	// it removes a STOPPED sandbox, REFUSES a RUNNING one (without -f), and ERRORS
	// on a nonexistent name — so its exit status, classified below, is what
	// decides whether the clone is safe to drop. There is no path that deletes the
	// clone without first attempting the atomic rm.
	if force {
		// --force: `sbx rm -f`. The user has accepted killing a running sandbox.
		// A not-found failure (the sandbox is already gone) is success; any other
		// failure aborts BEFORE deleting the checkout.
		if out, err := env.run("sbx", "rm", "-f", meta.Sandbox); err != nil {
			if probeTaskSandbox(env, meta.Sandbox) == sbxAbsent {
				return 0 // rm -f failed only because there was nothing to remove.
			}
			fmt.Fprintf(w, "pi-stack task rm: `sbx rm -f %s` failed; leaving clone intact at %s: %v\n%s", meta.Sandbox, co, err, out)
			return 1
		}
		return 0
	}
	// NON-FORCE: ALWAYS route through `sbx rm` WITHOUT -f — even when the fresh
	// probe read absent. sbx itself is the atomic guard: it refuses a sandbox that
	// came up after the probe. Never fall back to -f. On failure, re-probe ONCE to
	// classify why the rm failed:
	//   - absent  => the sandbox was already gone ("no such sandbox"); safe to
	//                proceed and delete the artifacts.
	//   - running => a live session owns the name; ABORT, leave the clone intact.
	//   - stopped => the sandbox is still present but rm failed; ABORT.
	//   - unknown => sbx can no longer be reached; ABORT (fail-safe).
	if out, err := env.run("sbx", "rm", meta.Sandbox); err != nil {
		switch probeTaskSandbox(env, meta.Sandbox) {
		case sbxAbsent:
			// The rm failed because the sandbox was not present; nothing was
			// removed and nothing live can be relying on the clone. Proceed.
		case sbxUnknown:
			fmt.Fprintf(w, "pi-stack task rm: `sbx rm %s` failed and the sandbox state can no longer be determined (sbx ls failed); leaving clone intact at %s: %v\n%s", meta.Sandbox, co, err, out)
			fmt.Fprintln(w, "Resolve (check `sbx ls`), then retry.")
			return 1
		default: // sbxRunning or sbxStopped: the sandbox is still present.
			fmt.Fprintf(w, "pi-stack task rm: `sbx rm %s` failed; the sandbox is likely still running. Leaving clone intact at %s: %v\n%s", meta.Sandbox, co, err, out)
			fmt.Fprintf(w, "Stop it first (sbx stop %s), or re-run with --force to kill it.\n", meta.Sandbox)
			return 1
		}
	}
	return 0
}

// gatherTaskState collects the four guard facts via the shellEnv seam (real git
// in production and integration tests, a fake sbx in tests).
func gatherTaskState(env shellEnv, m taskMeta, co string) taskState {
	st := taskState{}
	// Probe the sandbox ONCE and carry the tri-state; the guard and the teardown
	// executor both decide on this single value.
	st.sandbox = probeTaskSandbox(env, m.Sandbox)

	if out, err := env.run("git", "-C", co, "status", "--porcelain"); err == nil {
		st.dirty = strings.TrimSpace(out) != ""
	} else {
		// A failed status probe means the clean/dirty state is UNKNOWN, not clean.
		// Fail-safe: the guard refuses teardown without --force.
		st.unknown = true
	}

	// unrec: fetch ALL of the clone's local heads + HEAD (and its remote-tracking
	// refs) into a throwaway namespace in the main repo, count the commits that
	// live ONLY in the clone, then delete the throwaway namespace. "Would lose
	// work" = commits reachable from the clone's heads/HEAD but NOT reachable from
	// (any real main-repo ref) UNION (the clone's own refs/remotes/*). Covering
	// every head + HEAD catches commits on scratch branches and a detached HEAD,
	// not just the task branch; subtracting the clone's remotes keeps work that is
	// already pushed (and therefore recoverable) from counting as would-lose-work.
	name := strings.TrimPrefix(m.Branch, "pi-stack/")
	chk := "refs/pi-stack/_chk/" + name
	// Force refspecs (leading '+') so stale/existing _chk refs never block the
	// update and silently mask the count to 0.
	if _, err := env.run("git", "-C", m.Mainroot, "fetch", "-q", co,
		"+refs/heads/*:"+chk+"/heads/*",
		"+HEAD:"+chk+"/HEAD",
		"+refs/remotes/*:"+chk+"/remotes/*"); err == nil {
		// Positive set: the clone's heads + HEAD (under chk). Negative set: every
		// real main-repo ref (--exclude=refs/pi-stack/* keeps the throwaway refs and
		// any prior recovered snapshot out of the exclusion, so they don't mask the
		// count to 0) PLUS the clone's fetched remote-tracking refs. `--not` flips
		// the sense for everything after it; --exclude applies only to the next
		// --all, so the trailing --glob for remotes stays in the negated set.
		if out, err := env.run("git", "-C", m.Mainroot, "rev-list", "--count",
			"--glob="+chk+"/heads", chk+"/HEAD",
			"--not", "--exclude=refs/pi-stack/*", "--all",
			"--glob="+chk+"/remotes"); err == nil {
			st.unrec = parseCount(out)
		} else {
			st.unknown = true
		}
		// Delete the whole throwaway namespace (multiple refs now).
		if refs, err := env.run("git", "-C", m.Mainroot, "for-each-ref", "--format=%(refname)", chk); err == nil {
			for _, r := range strings.Fields(refs) {
				_, _ = env.run("git", "-C", m.Mainroot, "update-ref", "-d", r)
			}
		}
	} else {
		// A failed/blocked probe means unrec is UNKNOWN, not 0. Fail-safe: the
		// guard refuses teardown without --force.
		st.unknown = true
	}

	// unpushed: commits ahead of the upstream when one exists, else fall back to
	// unrec (defined for the no-upstream and no-remote cases).
	if out, err := env.run("git", "-C", co, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil && strings.TrimSpace(out) != "" {
		if c, err := env.run("git", "-C", co, "rev-list", "--count", "@{u}..HEAD"); err == nil {
			st.unpushed = parseCount(c)
		} else {
			// An upstream exists but counting ahead-commits failed: the unpushed
			// state is UNKNOWN, not 0. Fail-safe as above.
			st.unknown = true
		}
	} else {
		st.unpushed = st.unrec
	}
	return st
}

func parseCount(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// taskSandboxStatus returns the status column for name from `sbx ls` via the
// seam (mirrors sandboxStatus but hermetic-testable), or "" when absent. This
// is the DISPLAY-only accessor used by `task ls`; destructive decisions use the
// tri-state probeTaskSandbox instead, which never conflates errored with absent.
func taskSandboxStatus(env shellEnv, name string) string {
	if env.run == nil {
		return ""
	}
	out, err := env.run("sbx", "ls")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && f[0] == name {
			if len(f) >= 3 {
				return f[2]
			}
			return "exists"
		}
	}
	return ""
}

// sbxState is the tri-state a task probe resolves a sandbox to. The whole point
// is that an errored/unreachable `sbx` invocation is UNKNOWN, distinct from a
// clean "not in the list" ABSENT: callers must refuse destructive action on
// UNKNOWN rather than assume the safe-looking absent value.
type sbxState int

const (
	sbxUnknown sbxState = iota // could not determine (sbx errored / no runner)
	sbxAbsent                  // sbx responded and the name is not present
	sbxRunning                 // present, status column reads running
	sbxStopped                 // present, any other status
)

// probeTaskSandbox classifies name from `sbx ls` via the seam into one of
// {running, stopped, absent, unknown}. A non-zero/errored sbx invocation (or a
// missing runner) is UNKNOWN, never absent, so a failed probe can never be read
// as "the sandbox was never created".
func probeTaskSandbox(env shellEnv, name string) sbxState {
	if env.run == nil {
		return sbxUnknown
	}
	out, err := env.run("sbx", "ls")
	if err != nil {
		return sbxUnknown
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 1 && f[0] == name {
			if len(f) >= 3 && f[2] == "running" {
				return sbxRunning
			}
			return sbxStopped
		}
	}
	return sbxAbsent
}

// resolveMainroot returns the repository's git-common-dir as an absolute path
// via `git -C <cwd> rev-parse --path-format=absolute --git-common-dir`. This is
// the shared object/ref store for the repo and is stable across a linked
// worktree (all worktrees report the same common dir), a normal repo (its
// `.git` dir), and a bare repo (the repo dir itself). The returned path is what
// `task new` clones from AND the `-C` target for every rev-parse/fetch/update-ref
// against the main repo, so the two never disagree about which store they mean.
// The field is still called "mainroot" for continuity, but it now holds the
// git-common-dir, not the worktree root.
func resolveMainroot(env shellEnv, cwd string) (string, error) {
	out, err := env.run("git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	gitdir := strings.TrimSpace(out)
	if gitdir == "" {
		return "", fmt.Errorf("empty git-common-dir")
	}
	return gitdir, nil
}

// ---------------------------------------------------------------------------
// task new
// ---------------------------------------------------------------------------

func runTaskNew(env shellEnv, argv []string) {
	if wantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	name, from, passthrough, err := parseTaskNewArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task new: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task new: not a git repository: %v\n", err)
		os.Exit(1)
	}

	ref := from
	if ref == "" {
		ref = "HEAD"
	}
	// Resolve the ref to a concrete commit OID and check out THAT, not the ref
	// string. A local `git clone` does not copy the source's refs/remotes/* or
	// refs/pull/*, so a source-only ref would fail `checkout <ref>` in the clone;
	// the OID always resolves because --local hardlinks the whole object store.
	revOut, err := env.run("git", "-C", mainroot, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task new: start point %q not found in %s\n", ref, mainroot)
		os.Exit(1)
	}
	oid := strings.TrimSpace(revOut)
	if oid == "" {
		fmt.Fprintf(os.Stderr, "pi-stack task new: start point %q did not resolve to a commit\n", ref)
		os.Exit(1)
	}
	origin := ""
	if out, err := env.run("git", "-C", mainroot, "remote", "get-url", "origin"); err == nil {
		origin = strings.TrimSpace(out)
	}

	// The active profile namespaces the sandbox name (so contexts never collide).
	_, profile, _ := loadResolvedConfig()
	repokey := taskRepoKey(mainroot)
	sane := sanitizeTaskName(name)
	co, metaPath := taskPaths(repokey, sane)
	sbxname := taskSandboxName(repokey, name, profile)
	branch := "pi-stack/" + sane

	// Serialize the whole reserve -> clone -> checkout -> write-meta -> launch ->
	// rollback sequence for this (repokey, sane) behind the per-task lock, so a
	// concurrent `task new` or `task rm` on the same name is mutually exclusive
	// (closes R7-1 double-create and the R9-1 meta race). os.Exit skips defers, so
	// the locked fn NEVER calls os.Exit: it returns an exit code through exitCode
	// and we exit only after withTaskLock has released the lock.
	exitCode := 0
	lockErr := withTaskLock(repokey, sane, func() error {
		// Reserve co atomically. os.Mkdir fails with fs.ErrExist when the dir already
		// exists, so there is no TOCTOU window between "check" and "clone": whoever wins
		// the mkdir owns the directory, and a losing concurrent `task new` for the same
		// name never deletes the winner's live checkout. Only THIS invocation, when it
		// created co (owned), is ever allowed to roll it back.
		owned, err := reserveTaskCheckout(co)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				fmt.Fprintf(os.Stderr, "pi-stack task new: task %q already exists; `pi-stack run %s` to reattach\n", name, co)
			} else {
				fmt.Fprintf(os.Stderr, "pi-stack task new: reserve checkout: %v\n", err)
			}
			exitCode = 1
			return nil
		}
		rollback := func() {
			if owned {
				_ = os.RemoveAll(co)
			}
		}

		if out, err := env.run("git", "clone", "--local", mainroot, co); err != nil {
			rollback()
			fmt.Fprintf(os.Stderr, "pi-stack task new: git clone failed: %v\n%s", err, out)
			exitCode = 1
			return nil
		}
		if out, err := env.run("git", "-C", co, "checkout", "-q", "-b", branch, "--end-of-options", oid); err != nil {
			rollback()
			fmt.Fprintf(os.Stderr, "pi-stack task new: checkout failed: %v\n%s", err, out)
			exitCode = 1
			return nil
		}
		if origin != "" {
			// Wire origin to the real upstream so in-sandbox `git push` uses the sbx
			// credential proxy.
			_, _ = env.run("git", "-C", co, "remote", "set-url", "origin", origin)
		}
		if _, err := os.Stat(filepath.Join(mainroot, ".gitmodules")); err == nil {
			fmt.Fprintln(os.Stderr, "pi-stack task new: note: submodules are not auto-initialized (v1). "+
				"Run `git submodule update --init` in the sandbox and add submodule remotes to the network allowlist.")
		}

		meta := taskMeta{
			Name:     name,
			Mode:     "localclone",
			Sandbox:  sbxname,
			Mainroot: mainroot,
			Branch:   branch,
			Base:     ref,
			Origin:   origin,
			Profile:  profile,
			Created:  time.Now().UTC().Format(time.RFC3339),
		}
		if err := writeTaskMeta(metaPath, meta); err != nil {
			rollback()
			fmt.Fprintf(os.Stderr, "pi-stack task new: write metadata: %v\n", err)
			exitCode = 1
			return nil
		}

		fmt.Fprintf(os.Stderr, "pi-stack: task %q ready at %s (branch %s, sandbox %s)\n", name, co, branch, sbxname)

		o := runOpts{Workspace: co, Name: sbxname, Passthrough: passthrough}
		if err := taskLaunch(o); err != nil {
			switch probeTaskSandbox(env, sbxname) {
			case sbxAbsent:
				// CERTAIN the sandbox was never created: the clone holds no in-sandbox
				// work to lose, so roll it back cleanly (only if we created it).
				rollback()
				_ = os.Remove(metaPath)
				fmt.Fprintf(os.Stderr, "pi-stack task new: launch failed, rolled back clone: %v\n", err)
			case sbxUnknown:
				// Could NOT determine whether a sandbox exists: do not delete the clone,
				// or we might strand a live sandbox and drop its work. Leave everything
				// and hand the user the manual check.
				fmt.Fprintf(os.Stderr, "pi-stack task new: launch failed and the sandbox state is unknown (sbx ls did not respond): %v\n", err)
				fmt.Fprintf(os.Stderr, "Left the clone at %s. Check `sbx ls`, then remove %s and that clone manually if no sandbox was created.\n", co, sbxname)
			default:
				// running/stopped: the sandbox exists, the session merely exited
				// non-zero. Keep the clone; guarded `task rm` handles teardown safely.
				fmt.Fprintf(os.Stderr, "pi-stack task new: %v\n", err)
			}
			exitCode = 1
			return nil
		}
		return nil
	})
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task new: %v\n", lockErr)
		os.Exit(1)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// parseTaskNewArgs parses `new <name> [--from REF] [-- pi-args]`.
func parseTaskNewArgs(argv []string) (name, from string, passthrough []string, err error) {
	pre := argv
	for i, a := range argv {
		if a == "--" {
			pre = argv[:i]
			passthrough = append([]string(nil), argv[i+1:]...)
			break
		}
	}
	nameSet := false
	for i := 0; i < len(pre); i++ {
		a := pre[i]
		n := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			n = a[:eq]
		}
		switch {
		case n == "--from":
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				from = a[eq+1:]
			} else {
				if i+1 >= len(pre) {
					return "", "", nil, fmt.Errorf("flag --from needs a value")
				}
				i++
				from = pre[i]
			}
		case strings.HasPrefix(a, "-"):
			return "", "", nil, fmt.Errorf("unknown flag %q", a)
		default:
			if nameSet {
				return "", "", nil, fmt.Errorf("unexpected extra argument %q (use -- for pi args)", a)
			}
			name = a
			nameSet = true
		}
	}
	// A --from value that begins with '-' would be read by git as an option, not
	// a ref (it flows into rev-parse and checkout as a bare argv element).
	// Reject it here; the git calls also pass --end-of-options as defense in depth.
	if strings.HasPrefix(from, "-") {
		return "", "", nil, fmt.Errorf("--from value %q must not begin with '-'", from)
	}
	if !nameSet || strings.TrimSpace(name) == "" {
		return "", "", nil, fmt.Errorf("a task name is required")
	}
	return name, from, passthrough, nil
}

// prepareTaskLaunchSandbox resolves any pre-existing sandbox of the derived name
// before `task new` launches into it, mirroring executeTaskTeardown's atomicity
// (R5-1). It probes ONCE via the tri-state and:
//   - absent  -> nil: nothing in the way, proceed to a fresh create.
//   - running -> refuse: a live session owns this name; silently killing it (no
//     --force from the user) is exactly the probe-then-force-kill TOCTOU we avoid.
//   - unknown -> refuse: sbx could not be reached, so we cannot know it is safe
//     to remove; never force anything on an indeterminate probe.
//   - stopped -> `sbx rm <name>` WITHOUT -f. sbx atomically refuses a running
//     sandbox there, so a sandbox that started between the probe and the rm is
//     left alone (the rm fails) rather than force-killed. A failed rm ABORTS the
//     launch with a teachable message; we never fall back to -f.
func prepareTaskLaunchSandbox(env shellEnv, name string) error {
	switch probeTaskSandbox(env, name) {
	case sbxAbsent:
		return nil
	case sbxRunning:
		return fmt.Errorf("sandbox %q is already running; stop the session first (sbx stop %s), then retry", name, name)
	case sbxUnknown:
		return fmt.Errorf("cannot determine the state of sandbox %q (sbx ls failed); resolve (check `sbx ls`), then retry", name)
	default: // sbxStopped: recreate, but only via a non-force rm.
		if out, err := env.run("sbx", "rm", name); err != nil {
			return fmt.Errorf("could not remove the stopped sandbox %q; it may be running. "+
				"Stop it (sbx stop %s) or remove it, then retry: %v\n%s", name, name, err, strings.TrimRight(out, "\n"))
		}
		return nil
	}
}

// launchTask is the error-returning launch helper `task new` uses so a failed
// create can be rolled back. It reuses run.go's package helpers (config resolve,
// preflight, kit resolution, knowledge/profile wiring, buildSbxArgs) WITHOUT
// modifying run.go, and bypasses deriveSandboxName because o.Name is set.
func launchTask(o runOpts) error {
	if msg, block := modelProviderPreflight(defaultShellEnv()); block {
		return fmt.Errorf("%s", strings.TrimRight(msg, "\n"))
	}
	cfg, profile, err := loadResolvedConfig()
	if err != nil {
		return err
	}

	released := isReleased(version)
	kitOverride := len(o.Kits) > 0
	if o.Dev {
		root, err := resolveRepoRoot()
		if err != nil {
			return fmt.Errorf("--dev: %w", err)
		}
		o.DevRoot = root
		o.LocalKit = filepath.Join(root, "pi-kit")
		o.LocalImageTag = readLocalImageTag(root)
	} else if !released && !kitOverride {
		if root, err := resolveRepoRoot(); err == nil {
			o.LocalKit = filepath.Join(root, "pi-kit")
			o.LocalImageTag = readLocalImageTag(root)
		}
	}
	o.MCPEnabled = strings.TrimSpace(os.Getenv("SBX_MCP_URL")) != ""

	// Resolve any pre-existing sandbox of the derived name atomically before
	// launching into it (R5-1): a stopped one is removed with `sbx rm` (no -f),
	// a running or indeterminate one is refused. Never force-kill without --force.
	if err := prepareTaskLaunchSandbox(defaultShellEnv(), o.Name); err != nil {
		return err
	}

	wireKnowledgeScope(cfg, o.Workspace, defaultKnowledgeRPC())
	writeProfileFile(o.Workspace, profile)

	args := buildSbxArgs(cfg, o, version)
	if os.Getenv("PI_STACK_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "+ sbx "+strings.Join(args, " "))
	}
	cmd := exec.Command("sbx", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Run()
}

// ---------------------------------------------------------------------------
// task ls
// ---------------------------------------------------------------------------

type taskListRow struct {
	Name     string `json:"name"`
	Branch   string `json:"branch"`
	Sandbox  string `json:"sandbox"`
	Status   string `json:"status"`
	Dirty    bool   `json:"dirty"`
	Unpushed int    `json:"unpushed"`
	Unknown  bool   `json:"unknown"`
	Clone    string `json:"clone"`
}

func runTaskLs(env shellEnv, argv []string) {
	if wantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	jsonOut := false
	for _, a := range argv {
		switch a {
		case "--json":
			jsonOut = true
		default:
			fmt.Fprintf(os.Stderr, "pi-stack task ls: unknown argument %q\n\n%s", a, taskUsage)
			os.Exit(2)
		}
	}

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task ls: not a git repository: %v\n", err)
		os.Exit(1)
	}
	repokey := taskRepoKey(mainroot)
	metaDir := filepath.Join(taskStateRoot(), repokey, "meta")
	entries, _ := os.ReadDir(metaDir)

	var rows []taskListRow
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		m, err := readTaskMeta(filepath.Join(metaDir, e.Name()))
		if err != nil {
			continue
		}
		// Never trust stored branch/sandbox/mainroot for building refs or argv;
		// re-derive them from the validated name. Skip a meta that fails validation.
		fileBase := strings.TrimSuffix(e.Name(), ".json")
		m, err = hardenTaskMeta(m, mainroot, repokey, fileBase)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack task ls: skipping %s: %v\n", e.Name(), err)
			continue
		}
		co, _ := taskPaths(repokey, sanitizeTaskName(m.Name))
		st := gatherTaskState(env, m, co)
		rows = append(rows, taskListRow{
			Name:     m.Name,
			Branch:   m.Branch,
			Sandbox:  m.Sandbox,
			Status:   taskSandboxStatus(env, m.Sandbox),
			Dirty:    st.dirty,
			Unpushed: st.unpushed,
			Unknown:  st.unknown,
			Clone:    co,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })

	if jsonOut {
		if rows == nil {
			rows = []taskListRow{}
		}
		b, _ := json.MarshalIndent(rows, "", "  ")
		fmt.Println(string(b))
		return
	}

	if len(rows) == 0 {
		fmt.Println("No tasks for this repo.")
		fmt.Println("Next: `pi-stack task new <name>` to start one.")
		return
	}
	fmt.Printf("%-20s %-24s %-10s %s\n", "NAME", "BRANCH", "SANDBOX", "GIT")
	for _, r := range rows {
		status := r.Status
		if status == "" {
			status = "-"
		}
		var flags []string
		if r.Dirty {
			flags = append(flags, "dirty")
		}
		if r.Unpushed > 0 {
			flags = append(flags, fmt.Sprintf("%d unpushed", r.Unpushed))
		}
		git := "clean"
		if len(flags) > 0 {
			git = strings.Join(flags, ", ")
		}
		// An unknown git state (a probe failed) is neither clean nor a known count;
		// surface it explicitly rather than letting it read as clean/0.
		if r.Unknown {
			git = "unknown (probe failed)"
		}
		fmt.Printf("%-20s %-24s %-10s %s\n", r.Name, r.Branch, status, git)
	}
	fmt.Println("Next: `pi-stack task rm <name>` to tear one down (guarded).")
}

// ---------------------------------------------------------------------------
// task rm
// ---------------------------------------------------------------------------

func runTaskRm(env shellEnv, argv []string) {
	if wantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	name, force, err := parseTaskRmArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task rm: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task rm: not a git repository: %v\n", err)
		os.Exit(1)
	}
	repokey := taskRepoKey(mainroot)
	sane := sanitizeTaskName(name)
	co, metaPath := taskPaths(repokey, sane)

	// Serialize read-meta/harden -> gather -> guard -> snapshot -> sbx rm ->
	// delete-artifacts for this (repokey, sane) behind the per-task lock, so a
	// concurrent `task new`/`task rm` on the same name is mutually exclusive (this
	// is what stops teardown from deleting a concurrent `new`'s fresh metadata,
	// R9-1). os.Exit skips defers, so the locked fn NEVER exits: it returns an exit
	// code through exitCode and we exit only after the lock has been released.
	exitCode := 0
	lockErr := withTaskLock(repokey, sane, func() error {
		meta, err := readTaskMeta(metaPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack task rm: no such task %q for this repo\n", name)
			exitCode = 1
			return nil
		}
		// Never trust stored branch/sandbox/mainroot for building refs or the
		// `sbx rm` argv; re-derive them from the validated name. A meta that fails
		// validation is tampered or corrupt, so refuse rather than act on it.
		meta, err = hardenTaskMeta(meta, mainroot, repokey, sane)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack task rm: refusing to remove %q: %v\n", name, err)
			fmt.Fprintln(os.Stderr, "The task metadata looks tampered or corrupt; inspect it under the tasks state dir before retrying.")
			exitCode = 1
			return nil
		}

		st := gatherTaskState(env, meta, co)
		if msg, ok := taskRemoveGuard(st, force); !ok {
			fmt.Fprintf(os.Stderr, "pi-stack task rm: refusing to remove %q: %s.\n", name, msg)
			fmt.Fprintln(os.Stderr, "Recover first, or pass --force:")
			fmt.Fprintf(os.Stderr, "  git -C %s fetch %s %s\n", meta.Mainroot, co, meta.Branch)
			if meta.Origin != "" {
				fmt.Fprintf(os.Stderr, "  git -C %s push origin %s\n", co, meta.Branch)
			}
			// An operational refusal (running / dirty / would-lose-work / unknown-state)
			// is exit 1. Exit 2 is reserved for usage errors (bad flags, missing name).
			exitCode = 1
			return nil
		}

		// Teardown (never lose data, even with --force). executeTaskTeardown owns the
		// full order: snapshot -> fresh-probe message -> atomic rm (`sbx rm` no -f for
		// non-force, `sbx rm -f` for --force), deciding on the SAME shellEnv the guard's
		// facts came from. A non-zero return means it aborted with everything intact;
		// do NOT touch the checkout.
		recovered := "refs/pi-stack/recovered/" + sane
		if rc := executeTaskTeardown(env, os.Stderr, meta, co, name, recovered, force, st); rc != 0 {
			exitCode = rc
			return nil
		}
		// c. remove checkout + metadata. The main repo's branches are never touched.
		// If either removal fails, name what remains and exit non-zero rather than
		// print a false "Removed" claim.
		if err := removeTaskArtifacts(co, metaPath, os.RemoveAll, os.Remove); err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack task rm: sandbox %s was torn down but %v\n", meta.Sandbox, err)
			fmt.Fprintln(os.Stderr, "Remove the leftover state by hand before reusing this task name.")
			exitCode = 1
			return nil
		}

		fmt.Printf("Removed task %q (sandbox %s).\n", name, meta.Sandbox)
		fmt.Printf("Next: recovered commits (if any) are at %s in %s.\n", recovered, meta.Mainroot)
		return nil
	})
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "pi-stack task rm: %v\n", lockErr)
		os.Exit(1)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// removeTaskArtifacts deletes the task's clone dir and metadata file, returning
// an error naming whatever remains when either delete fails. The remove funcs
// are seams so a test can inject a failure without simulating FS permissions. A
// path that is already gone (IsNotExist) is treated as success.
func removeTaskArtifacts(co, metaPath string, removeAll func(string) error, remove func(string) error) error {
	var stuck []string
	if err := removeAll(co); err != nil && !os.IsNotExist(err) {
		stuck = append(stuck, fmt.Sprintf("the clone remains at %s (%v)", co, err))
	}
	if err := remove(metaPath); err != nil && !os.IsNotExist(err) {
		stuck = append(stuck, fmt.Sprintf("the metadata remains at %s (%v)", metaPath, err))
	}
	if len(stuck) > 0 {
		return fmt.Errorf("%s", strings.Join(stuck, "; "))
	}
	return nil
}

// parseTaskRmArgs parses `rm <name> [--force]`.
func parseTaskRmArgs(argv []string) (name string, force bool, err error) {
	nameSet := false
	for _, a := range argv {
		switch {
		case a == "--force" || a == "-f":
			force = true
		case strings.HasPrefix(a, "-"):
			return "", false, fmt.Errorf("unknown flag %q", a)
		default:
			if nameSet {
				return "", false, fmt.Errorf("unexpected extra argument %q", a)
			}
			name = a
			nameSet = true
		}
	}
	if !nameSet || strings.TrimSpace(name) == "" {
		return "", false, fmt.Errorf("a task name is required")
	}
	return name, force, nil
}
