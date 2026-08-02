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
	"pix/host/monitor/tui"
	"sort"
	"strconv"
	"strings"
	"time"
)

const taskUsage = `usage: pix task <new|ls|path|rm|gc|harvest> [args]

Run parallel tasks on one repo. Each task is a local clone with its own branch
and sandbox, so tasks never collide. Commits land in the task's clone on this
host and can be pushed or fetched back.

  new <name> [--from REF] [-- pi-args]   clone + branch + launch a sandbox
  ls  [--json]                           tasks, their branch, sandbox + git state
  path <name>                            print the task's checkout dir (for cd)
  rm  <name> [--force]                   tear down sandbox + clone (guarded)
  gc  [--days N] [--dry-run] [--no-harvest] [--artifact-days N]
                                         prune clean tasks older than N days (default 7)
  harvest <name> [--to DIR]              copy uncommitted docs to a durable dir

Clones live under $XDG_STATE_HOME/pix/tasks/<repo>/co/<name>, outside your
repo. ` + "`pix task rm`" + ` refuses to drop uncommitted or unpushed work.
Harvested docs live under $XDG_DATA_HOME/pix/artifacts, so they survive a
` + "`pix state reset`" + `.
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
	case "path":
		runTaskPath(defaultShellEnv(), argv[1:])
	case "rm", "remove":
		runTaskRm(defaultShellEnv(), argv[1:])
	case "gc":
		runTaskGc(defaultShellEnv(), argv[1:])
	case "harvest":
		runTaskHarvest(defaultShellEnv(), argv[1:])
	default:
		// Support the `pix task <name> path` grammar (name-then-verb) the
		// user reaches for, so `cd "$(pix task foo path)"` reads naturally.
		// The canonical form is `task path <name>`; this is a convenience alias.
		if len(argv) == 2 && argv[1] == "path" {
			runTaskPath(defaultShellEnv(), argv[:1])
			return
		}
		fmt.Fprintf(os.Stderr, "pix task: unknown subcommand %q\n\n%s", argv[0], taskUsage)
		os.Exit(2)
	}
}

// ---------------------------------------------------------------------------
// Pure helpers (name / path / key). No filesystem writes, no exec.
// ---------------------------------------------------------------------------

// maxTaskNameLen caps the sanitized <name> segment before it is composed into
// the full sandbox name. An overflow is truncated and tagged with a short hash
// of the full name so it stays unique. The COMPOSITE name is bounded separately
// by boundSandboxName against maxSandboxNameLen; this per-segment cap only keeps
// a single pathological name from dominating the whole budget.
const maxTaskNameLen = 40

// maxSandboxNameLen is the hard cap on the composed sandbox name
// (pix-t-<label>-<repokey>-<name>[-<profile>]). 63 is the strictest common
// limit (RFC1123 label); sbx never enforced a total, so this is the safe
// conservative bound. boundSandboxName trims to fit: name first, then label,
// NEVER the repokey (the uniqueness guarantee).
const maxSandboxNameLen = 63

// maxRepoLabelLen caps the human repo label. The label is a legibility hint, not
// an identifier (the repokey guarantees uniqueness), so an overflow is a plain
// truncation with no hash tag.
const maxRepoLabelLen = 12

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

// taskRepoLabel is the human-readable repo hint stamped into the sandbox name
// and the on-disk path so `task ls`, `sbx ls`, and a browse of the state dir are
// legible without the user having to prefix every task name with the repo. It is
// the sanitized basename of the repo (the git-common-dir's parent for a normal
// repo, or the dir itself for a bare repo), capped at maxRepoLabelLen. It is a
// hint only: two repos that both basename to "api" still differ by their
// repokey, so the label never has to be unique and overflow is a plain truncate.
func taskRepoLabel(mainroot string) string {
	p := mainroot
	if abs, err := filepath.Abs(mainroot); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	// mainroot is the git-common-dir. For a normal repo that ends in "/.git", so
	// the meaningful name is its parent; for a bare repo it is the repo dir itself
	// (often ending ".git"). Strip a trailing ".git" and a ".git" basename's parent
	// so both shapes yield the working repo name.
	base := filepath.Base(p)
	if base == ".git" {
		base = filepath.Base(filepath.Dir(p))
	}
	base = strings.TrimSuffix(base, ".git")
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "repo"
	}
	if len(out) > maxRepoLabelLen {
		out = strings.TrimRight(out[:maxRepoLabelLen], "-")
		if out == "" {
			out = "repo"
		}
	}
	return out
}

// taskRepoDir is the per-repo state dir segment: "<label>-<repokey>". Browsing
// $STATE/pix/tasks/ is then readable (e.g. "pix-a1b2c3d4") while the
// repokey still guarantees two same-named repos never collide.
func taskRepoDir(mainroot string) string {
	return taskRepoLabel(mainroot) + "-" + taskRepoKey(mainroot)
}

// taskLayout names a per-repo state dir and whether it is the legacy
// (pre-label) shape. legacy drives which sandbox-name formula a task's metadata
// hardens to, so rm/gc/ls target the sandbox the task ACTUALLY owns.
type taskLayout struct {
	dir    string
	legacy bool
}

// hasTaskMetaDir reports whether dir (under the state root) is a populated repo
// state dir, i.e. it has a meta/ subdir. Checking meta/ (not just the dir) means
// an unrelated sibling dir — e.g. a locks/ tree — never reads as a layout.
func hasTaskMetaDir(dir string) bool {
	fi, err := os.Stat(filepath.Join(taskStateRoot(), dir, "meta"))
	return err == nil && fi.IsDir()
}

// existingTaskLayouts returns every populated state layout for a repo: the new
// "<label>-<repokey>" dir and/or the legacy bare "<repokey>" dir. ls/gc iterate
// this UNION so a legacy task never becomes invisible once a new-layout task is
// created (the pre-label-migration bug).
func existingTaskLayouts(mainroot string) []taskLayout {
	newer := taskRepoDir(mainroot)
	legacy := taskRepoKey(mainroot)
	var out []taskLayout
	if hasTaskMetaDir(newer) {
		out = append(out, taskLayout{newer, false})
	}
	if legacy != newer && hasTaskMetaDir(legacy) {
		out = append(out, taskLayout{legacy, true})
	}
	return out
}

// findTaskLayout locates the layout holding task `sane` for the current repo,
// preferring the new labeled layout. found=false means no such task. A task that
// exists in BOTH layouts is ambiguous (two distinct sandboxes) and callers must
// refuse rather than guess which one to act on.
func findTaskLayout(mainroot, sane string) (lay taskLayout, found, ambiguous bool) {
	newer := taskRepoDir(mainroot)
	legacy := taskRepoKey(mainroot)
	_, newMeta := taskPaths(newer, sane)
	_, legMeta := taskPaths(legacy, sane)
	newOK := fileExistsTask(newMeta)
	legOK := legacy != newer && fileExistsTask(legMeta)
	switch {
	case newOK && legOK:
		return taskLayout{}, false, true
	case newOK:
		return taskLayout{newer, false}, true, false
	case legOK:
		return taskLayout{legacy, true}, true, false
	default:
		return taskLayout{newer, false}, false, false
	}
}

func fileExistsTask(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// taskStateRoot is the base dir for all task state:
// $XDG_STATE_HOME/pix/tasks (default ~/.local/state/pix/tasks).
func taskStateRoot() string {
	if x := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); x != "" {
		return filepath.Join(x, "pix", "tasks")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "state", "pix", "tasks")
}

// taskPaths resolves the checkout dir and metadata file for a task. repoDir is
// the per-repo state dir segment (new "<label>-<repokey>" or legacy "<repokey>")
// and name is expected to be already sanitized by the caller.
func taskPaths(repoDir, name string) (co, meta string) {
	base := filepath.Join(taskStateRoot(), repoDir)
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

// taskLockPath is the per-task advisory lock file, keyed by the CANONICAL repokey
// (NOT the layout dir): $STATE/pix/tasks/<repokey>/locks/<name>.lock. Keying
// by repokey makes a legacy task and a new-layout task of the same name mutually
// exclusive (a `task new` cannot race a legacy `task rm`). The dir has no meta/
// subdir, so hasTaskMetaDir never mistakes it for a legacy layout. It sits
// OUTSIDE the co/ and meta/ trees `task rm` deletes, so it is never a removed
// artifact (tiny and harmless to leave behind).
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
	// Shared flock helper (serve_start.go) — the same dance the serve spawn lock
	// uses, factored so it is written once.
	return withFlock(taskLockPath(repokey, name), fn)
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
// "pix-t-" + label + "-" + repokey + "-" + sanitize(name) [+ "-" + profile].
// The label is a human hint; the repokey is the uniqueness guarantee. The
// profile suffix is added only for a NAMED (non-default) profile, mirroring
// run.go. The composed name is bounded by boundSandboxName so it always stays
// within maxSandboxNameLen.
func taskSandboxName(label, repokey, name, profile string) string {
	_ = profile // profiles removed; the parameter is retained for call-site stability
	return boundSandboxName(label, repokey, sanitizeTaskName(name), "")
}

// legacyTaskSandboxName is the PRE-LABEL sandbox-name formula
// ("pix-t-<repokey>-<name>[-<profile>]"). Tasks created before the repo
// label was introduced live in the bare-<repokey> state dir and own a sandbox
// with THIS name, so rm/gc/ls must derive it (not the new labeled name) or they
// would delete a clone while leaving its real sandbox running.
func legacyTaskSandboxName(repokey, name, profile string) string {
	_ = profile // profiles removed; retained for call-site stability
	return "pix-t-" + repokey + "-" + sanitizeTaskName(name)
}

// boundSandboxName composes "pix-t-<label>-<repokey>-<name>[-<prof>]" and,
// if it exceeds maxSandboxNameLen, trims to fit in priority order: the name
// first (hash-tagged so it stays unique), then the label (a cosmetic hint),
// NEVER the repokey (correctness). prof is already sanitized and may be empty.
// name and label are already sanitized by the caller.
func boundSandboxName(label, repokey, name, prof string) string {
	compose := func(l, n, p string) string {
		s := "pix-t-" + l + "-" + repokey + "-" + n
		if p != "" {
			s += "-" + p
		}
		return s
	}
	if len(compose(label, name, prof)) <= maxSandboxNameLen {
		return compose(label, name, prof)
	}
	// 1. Trim the name (hash-tag it so a truncated name is still unique), keeping
	// the full label + profile, down to a floor so a huge label can't erase the name.
	for n := len(name); n >= nameTrimFloor; n-- {
		cand := hashTagTrim(name, n)
		if len(compose(label, cand, prof)) <= maxSandboxNameLen {
			return compose(label, cand, prof)
		}
	}
	// 2. Name is at its floor; now trim the label (a plain cosmetic truncation).
	nameFloor := hashTagTrim(name, nameTrimFloor)
	for l := len(label); l >= 1; l-- {
		cand := strings.TrimRight(label[:l], "-")
		if cand == "" {
			continue
		}
		if len(compose(cand, nameFloor, prof)) <= maxSandboxNameLen {
			return compose(cand, nameFloor, prof)
		}
	}
	// 3. Drop the label entirely (repokey still guarantees uniqueness), then, if an
	// absurdly long profile STILL overflows, hash-tag-trim the profile too. The
	// repokey is never touched, so distinct tasks never collide.
	if len(compose("repo", nameFloor, prof)) <= maxSandboxNameLen {
		return compose("repo", nameFloor, prof)
	}
	for p := len(prof); p >= 0; p-- {
		var cand string
		if p > 0 {
			cand = hashTagTrim(prof, max(p, nameTrimFloor))
			if len(cand) > p && p < nameTrimFloor {
				continue
			}
		}
		if len(compose("repo", nameFloor, cand)) <= maxSandboxNameLen {
			return compose("repo", nameFloor, cand)
		}
	}
	// Unreachable given the fixed-width prefix + repokey fit within 63, but never
	// return an over-bound name: hard-truncate as the absolute last resort.
	s := compose("repo", nameFloor, "")
	if len(s) > maxSandboxNameLen {
		s = s[:maxSandboxNameLen]
	}
	return s
}

// nameTrimFloor is the minimum trimmed-name length: hashTagTrim needs the 10-hex
// tag + dash (11) plus at least 1 prefix rune.
const nameTrimFloor = 12

// hashTagTrim truncates s to n runes, appending a 10-hex tag of the ORIGINAL s
// so two different names that truncate to the same prefix stay distinct (a
// 40-bit tag makes a birthday collision astronomically unlikely across the
// handful of tasks a repo ever has). n must be >= nameTrimFloor. A string
// already <= n is returned unchanged.
func hashTagTrim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < nameTrimFloor {
		n = nameTrimFloor
	}
	sum := sha256.Sum256([]byte(s))
	return s[:n-11] + "-" + hex.EncodeToString(sum[:])[:10]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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
	// Repo is the human repo label. Display/path only and re-derived from mainroot
	// in hardenTaskMeta (never trusted from disk), so an old meta without it just
	// works and a tampered value can never steer a path or argv.
	Repo string `json:"repo,omitempty"`
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
// The Profile field is a HOLDOVER from when profiles namespaced the sandbox
// name (see taskSandboxName/legacyTaskSandboxName, which now ignore it — the
// active PACK is the unit of context, see docs/design/packs.md). Every task
// written by the current `task new` stores it as "" (loadResolvedConfig always
// returns "" now), and an OLDER meta may carry a real profile name like
// "work": both are accepted here without requiring either. Rejecting an empty
// Profile was the actual bug (AC-P0-008): it hid EVERY task the current writer
// creates, because the writer and this guard disagreed about what "no profile"
// means.
func hardenTaskMeta(m taskMeta, mainroot, repokey string, legacy bool, fileBase string) (taskMeta, error) {
	sane := sanitizeTaskName(m.Name)
	if sane != fileBase {
		return m, fmt.Errorf("metadata name %q does not match its file %q.json", m.Name, fileBase)
	}
	label := taskRepoLabel(mainroot)
	m.Mainroot = mainroot
	m.Repo = label
	m.Branch = "pix/" + sane
	// The sandbox name is derived from the state LAYOUT (cwd-derived, trusted),
	// NOT the stored name: a legacy task in the bare-<repokey> dir owns the
	// pre-label sandbox name; a new-layout task owns the labeled name. Deriving the
	// wrong one would let rm/gc delete a clone while its real sandbox lives on.
	if legacy {
		m.Sandbox = legacyTaskSandboxName(repokey, m.Name, m.Profile)
	} else {
		m.Sandbox = taskSandboxName(label, repokey, m.Name, m.Profile)
	}
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
// the branch to refs/pix/recovered/<name>, re-probe the sandbox for a friendly
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
	// branch. recovered is the namespace prefix (refs/pix/recovered/<name>).
	snapshotArgs := []string{"-C", meta.Mainroot, "fetch", "-q", co,
		"+refs/heads/*:" + recovered + "/heads/*",
		"+HEAD:" + recovered + "/HEAD"}
	if out, err := env.Run("git", snapshotArgs...); err != nil {
		fmt.Fprintf(w, "pix task rm: warning: could not snapshot to %s: %v\n%s", recovered, err, out)
		// --force still must not silently drop unrecoverable work: if the snapshot
		// failed AND the work might be unrecoverable (clone-only commits, or an
		// UNKNOWN unrec state), abort and leave the clone intact.
		if taskForceSnapshotAbort(force, st.unrec, st.unknown, true) {
			if st.unknown {
				fmt.Fprintf(w, "pix task rm: refusing to remove %q: the recovery snapshot failed and this clone's unrecovered-commit state is unknown (git probe failed).\n", name)
			} else {
				fmt.Fprintf(w, "pix task rm: refusing to remove %q: the recovery snapshot failed and %d commit(s) live only in this clone.\n", name, st.unrec)
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
			fmt.Fprintf(w, "pix task rm: refusing to remove %q: the sandbox is now running; stop the session first (sbx stop %s).\n", name, meta.Sandbox)
		} else {
			fmt.Fprintf(w, "pix task rm: refusing to remove %q: cannot determine the state of sandbox %q (sbx ls failed); leaving clone intact at %s.\n", name, meta.Sandbox, co)
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
		if out, err := env.Run("sbx", "rm", "-f", meta.Sandbox); err != nil {
			if probeTaskSandbox(env, meta.Sandbox) == sbxAbsent {
				// rm -f failed only because there was nothing to remove — the
				// lifetime is positively over, so the receipt goes too.
				clearTaskSandboxReceipt(w, meta.Sandbox)
				return 0
			}
			fmt.Fprintf(w, "pix task rm: `sbx rm -f %s` failed; leaving clone intact at %s: %v\n%s", meta.Sandbox, co, err, out)
			return 1
		}
		clearTaskSandboxReceipt(w, meta.Sandbox)
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
	if out, err := env.Run("sbx", "rm", meta.Sandbox); err != nil {
		switch probeTaskSandbox(env, meta.Sandbox) {
		case sbxAbsent:
			// The rm failed because the sandbox was not present; nothing was
			// removed and nothing live can be relying on the clone. Proceed.
		case sbxUnknown:
			// NOTE: every ABORT path below (unknown/running/stopped) RETAINS the
			// MCP receipt — the sandbox was not positively removed.
			fmt.Fprintf(w, "pix task rm: `sbx rm %s` failed and the sandbox state can no longer be determined (sbx ls failed); leaving clone intact at %s: %v\n%s", meta.Sandbox, co, err, out)
			fmt.Fprintln(w, "Resolve (check `sbx ls`), then retry.")
			return 1
		default: // sbxRunning or sbxStopped: the sandbox is still present.
			fmt.Fprintf(w, "pix task rm: `sbx rm %s` failed; the sandbox is likely still running. Leaving clone intact at %s: %v\n%s", meta.Sandbox, co, err, out)
			fmt.Fprintf(w, "Stop it first (sbx stop %s), or re-run with --force to kill it.\n", meta.Sandbox)
			return 1
		}
	}
	// Removal succeeded (or the sandbox was positively confirmed gone): the
	// lifetime is over, so its MCP receipt is stale evidence — clear it (E).
	clearTaskSandboxReceipt(w, meta.Sandbox)
	return 0
}

// clearTaskSandboxReceipt clears the launcher MCP receipt after a task path
// POSITIVELY removed (or confirmed gone) its sandbox. Best-effort with a
// warning to w: the removal itself already succeeded, and the next launcher
// create's pre-create clear is the correctness backstop.
func clearTaskSandboxReceipt(w io.Writer, sandbox string) {
	if err := clearRemovedSandboxReceipt(sandbox); err != nil {
		fmt.Fprintf(w, "pix task rm: warning: could not clear the mcp receipt for removed sandbox %s: %v\n", sandbox, err)
	}
}

// gatherTaskState collects the four guard facts via the shellEnv seam (real git
// in production and integration tests, a fake sbx in tests).
func gatherTaskState(env shellEnv, m taskMeta, co string) taskState {
	st := taskState{}
	// Probe the sandbox ONCE and carry the tri-state; the guard and the teardown
	// executor both decide on this single value.
	st.sandbox = probeTaskSandbox(env, m.Sandbox)

	if out, err := env.Run("git", "-C", co, "status", "--porcelain"); err == nil {
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
	name := strings.TrimPrefix(m.Branch, "pix/")
	chk := "refs/pix/_chk/" + name
	// Force refspecs (leading '+') so stale/existing _chk refs never block the
	// update and silently mask the count to 0.
	if _, err := env.Run("git", "-C", m.Mainroot, "fetch", "-q", co,
		"+refs/heads/*:"+chk+"/heads/*",
		"+HEAD:"+chk+"/HEAD",
		"+refs/remotes/*:"+chk+"/remotes/*"); err == nil {
		// Positive set: the clone's heads + HEAD (under chk). Negative set: every
		// real main-repo ref (--exclude=refs/pix/* keeps the throwaway refs and
		// any prior recovered snapshot out of the exclusion, so they don't mask the
		// count to 0) PLUS the clone's fetched remote-tracking refs. `--not` flips
		// the sense for everything after it; --exclude applies only to the next
		// --all, so the trailing --glob for remotes stays in the negated set.
		if out, err := env.Run("git", "-C", m.Mainroot, "rev-list", "--count",
			"--glob="+chk+"/heads", chk+"/HEAD",
			"--not", "--exclude=refs/pix/*", "--all",
			"--glob="+chk+"/remotes"); err == nil {
			st.unrec = parseCount(out)
		} else {
			st.unknown = true
		}
		// Delete the whole throwaway namespace (multiple refs now).
		if refs, err := env.Run("git", "-C", m.Mainroot, "for-each-ref", "--format=%(refname)", chk); err == nil {
			for _, r := range strings.Fields(refs) {
				_, _ = env.Run("git", "-C", m.Mainroot, "update-ref", "-d", r)
			}
		}
	} else {
		// A failed/blocked probe means unrec is UNKNOWN, not 0. Fail-safe: the
		// guard refuses teardown without --force.
		st.unknown = true
	}

	// unpushed: commits ahead of the upstream when one exists, else fall back to
	// unrec (defined for the no-upstream and no-remote cases).
	if out, err := env.Run("git", "-C", co, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err == nil && strings.TrimSpace(out) != "" {
		if c, err := env.Run("git", "-C", co, "rev-list", "--count", "@{u}..HEAD"); err == nil {
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
	// BOUNDED (probeRun): a hung `sbx ls` yields "" (no display status), it
	// never wedges `task ls`.
	out, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
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
// as "the sandbox was never created". BOUNDED (probeRun): a hung sbx times out
// to UNKNOWN — run/setup/task preflights degrade honestly instead of wedging.
func probeTaskSandbox(env shellEnv, name string) sbxState {
	out, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
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
	out, err := env.Run("git", "-C", cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
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
		fmt.Fprintf(os.Stderr, "pix task new: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task new: not a git repository: %v\n", err)
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
	revOut, err := env.Run("git", "-C", mainroot, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task new: start point %q not found in %s\n", ref, mainroot)
		os.Exit(1)
	}
	oid := strings.TrimSpace(revOut)
	if oid == "" {
		fmt.Fprintf(os.Stderr, "pix task new: start point %q did not resolve to a commit\n", ref)
		os.Exit(1)
	}
	origin := ""
	if out, err := env.Run("git", "-C", mainroot, "remote", "get-url", "origin"); err == nil {
		origin = strings.TrimSpace(out)
	}

	// The active profile namespaces the sandbox name (so contexts never collide).
	_, profile, _ := loadResolvedConfig()
	repokey := taskRepoKey(mainroot)
	label := taskRepoLabel(mainroot)
	repoDir := taskRepoDir(mainroot) // `new` always writes the new <label>-<key> layout
	sane := sanitizeTaskName(name)
	co, metaPath := taskPaths(repoDir, sane)
	sbxname := taskSandboxName(label, repokey, name, profile)
	branch := "pix/" + sane

	// Serialize the whole reserve -> clone -> checkout -> write-meta -> launch ->
	// rollback sequence for this (repokey, sane) behind the per-task lock, so a
	// concurrent `task new` or `task rm` on the same name is mutually exclusive
	// (closes R7-1 double-create and the R9-1 meta race). os.Exit skips defers, so
	// the locked fn NEVER calls os.Exit: it returns an exit code through exitCode
	// and we exit only after withTaskLock has released the lock.
	exitCode := 0
	lockErr := withTaskLock(repokey, sane, func() error {
		// Refuse if a task of this name already exists in EITHER layout (a legacy
		// bare-<key> task or a leftover new-layout one), so `new` never shadows a
		// legacy task or races its removal. The new-layout dir collision is also
		// caught atomically by reserveTaskCheckout below; this catches the legacy case.
		if lay, found, ambiguous := findTaskLayout(mainroot, sane); found || ambiguous {
			where := "this repo"
			switch {
			case ambiguous:
				where = "BOTH the new and legacy task layout for this repo"
			case lay.legacy:
				where = "the legacy task layout for this repo"
			}
			fmt.Fprintf(os.Stderr, "pix task new: task %q already exists in %s; `pix run` its clone to reattach, or `pix task rm %s` first\n", name, where, name)
			exitCode = 1
			return nil
		}
		// Reserve co atomically. os.Mkdir fails with fs.ErrExist when the dir already
		// exists, so there is no TOCTOU window between "check" and "clone": whoever wins
		// the mkdir owns the directory, and a losing concurrent `task new` for the same
		// name never deletes the winner's live checkout. Only THIS invocation, when it
		// created co (owned), is ever allowed to roll it back.
		owned, err := reserveTaskCheckout(co)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				fmt.Fprintf(os.Stderr, "pix task new: task %q already exists; `pix run %s` to reattach\n", name, co)
			} else {
				fmt.Fprintf(os.Stderr, "pix task new: reserve checkout: %v\n", err)
			}
			exitCode = 1
			return nil
		}
		rollback := func() {
			if owned {
				_ = os.RemoveAll(co)
			}
		}

		if out, err := env.Run("git", "clone", "--local", mainroot, co); err != nil {
			rollback()
			fmt.Fprintf(os.Stderr, "pix task new: git clone failed: %v\n%s", err, out)
			exitCode = 1
			return nil
		}
		if out, err := env.Run("git", "-C", co, "checkout", "-q", "-b", branch, "--end-of-options", oid); err != nil {
			rollback()
			fmt.Fprintf(os.Stderr, "pix task new: checkout failed: %v\n%s", err, out)
			exitCode = 1
			return nil
		}
		if origin != "" {
			// Wire origin to the real upstream so in-sandbox `git push` uses the sbx
			// credential proxy.
			_, _ = env.Run("git", "-C", co, "remote", "set-url", "origin", origin)
		}
		if _, err := os.Stat(filepath.Join(mainroot, ".gitmodules")); err == nil {
			fmt.Fprintln(os.Stderr, "pix task new: note: submodules are not auto-initialized (v1). "+
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
			Repo:     label,
		}
		if err := writeTaskMeta(metaPath, meta); err != nil {
			rollback()
			fmt.Fprintf(os.Stderr, "pix task new: write metadata: %v\n", err)
			exitCode = 1
			return nil
		}

		fmt.Fprintf(os.Stderr, "pix: task %q ready at %s (branch %s, sandbox %s)\n", name, co, branch, sbxname)

		o := runOpts{Workspace: co, Name: sbxname, Passthrough: passthrough}
		if err := taskLaunch(o); err != nil {
			switch probeTaskSandbox(env, sbxname) {
			case sbxAbsent:
				// CERTAIN the sandbox was never created: the clone holds no in-sandbox
				// work to lose, so roll it back cleanly (only if we created it).
				rollback()
				_ = os.Remove(metaPath)
				fmt.Fprintf(os.Stderr, "pix task new: launch failed, rolled back clone: %v\n", err)
			case sbxUnknown:
				// Could NOT determine whether a sandbox exists: do not delete the clone,
				// or we might strand a live sandbox and drop its work. Leave everything
				// and hand the user the manual check.
				fmt.Fprintf(os.Stderr, "pix task new: launch failed and the sandbox state is unknown (sbx ls did not respond): %v\n", err)
				fmt.Fprintf(os.Stderr, "Left the clone at %s. Check `sbx ls`, then remove %s and that clone manually if no sandbox was created.\n", co, sbxname)
			default:
				// running/stopped: the sandbox exists, the session merely exited
				// non-zero. Keep the clone; guarded `task rm` handles teardown safely.
				fmt.Fprintf(os.Stderr, "pix task new: %v\n", err)
			}
			exitCode = 1
			return nil
		}
		return nil
	})
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "pix task new: %v\n", lockErr)
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
		if out, err := env.Run("sbx", "rm", name); err != nil {
			return fmt.Errorf("could not remove the stopped sandbox %q; it may be running. "+
				"Stop it (sbx stop %s) or remove it, then retry: %v\n%s", name, name, err, strings.TrimRight(out, "\n"))
		}
		// Launcher-side removal succeeded: the old lifetime's MCP receipt is
		// dead — clear it (E). Best-effort; the new create clears again anyway.
		clearTaskSandboxReceipt(os.Stderr, name)
		return nil
	}
}

// launchTask is the error-returning launch helper `task new` uses so a failed
// create can be rolled back. It reuses run.go's package helpers (config resolve,
// preflight, kit resolution, knowledge/profile wiring, buildSbxArgs) WITHOUT
// modifying run.go, and bypasses deriveSandboxName because o.Name is set.
func launchTask(o runOpts) error {
	env := defaultShellEnv()
	if _, err := env.LookPath("sbx"); err == nil && !configuredKeylessInference() {
		// Resolve any 1Password key refs into sbx first (same no-ritual path as run),
		// so a task on a fresh machine isn't rejected for a key it can auto-provision.
		ensureProviderKeysFromRefs(env, os.Stderr)
		// Tri-state (same as run): refuse ONLY when we can POSITIVELY confirm no key.
		// A transient `sbx secret ls` failure (probeOK=false) must not abort a task.
		if present, probeOK := sbxModelKeyState(env); probeOK && !present {
			return fmt.Errorf("%s", strings.TrimRight(modelKeyMissingMessage(env), "\n"))
		}
	}
	cfg, _, err := loadResolvedConfig()
	if err != nil {
		return err
	}
	if applied, rerr := applyConfiguredSessionModel(&o, cfg); rerr != nil {
		fmt.Fprintf(os.Stderr, "pix: run_intent %q did not resolve (%v); using pi's default model. Fix with `pix config set run_intent <intent>`.\n", strings.TrimSpace(cfg.RunIntent), rerr)
	} else if applied && o.Model != "" {
		fmt.Fprintf(os.Stderr, "pix: intent %q -> model %s\n", o.Intent, o.Model)
	}
	if !inferenceAllowsModel(cfg, o.Model) {
		return fmt.Errorf("model %q is not available through the configured inference backends", o.Model)
	}

	// A task is a fresh sandbox; mount the active pack (skills + model pref) so it
	// gets the same authored context a normal `pix run` does. Fatal on error
	// (round-4 F2): a declared-but-unbuildable pack wrapper refuses the launch.
	// effectivePack is what actually loaded/applied — "" when there is no active
	// pack OR applyPackToLaunch degraded via errNotAPack — and is what the
	// sandbox.pack marker + memory scope below must agree on (never the merely
	// CONFIGURED activePackRoot(cfg.Pack, o.Pack)).
	effectivePack, err := applyPackStackToLaunch(cfg, &o, defaultShellEnv())
	if err != nil {
		return err
	}
	var generatedKitDirs []string
	defer func() {
		if err := cleanupGeneratedKitDirs(generatedKitDirs); err != nil {
			fmt.Fprintf(os.Stderr, "pix: warning: %v\n", err)
		}
	}()
	if kit, ierr := synthesizeInferenceKit(cfg); ierr != nil {
		return fmt.Errorf("inference: %w", ierr)
	} else if kit != "" {
		o.PackKits = append(o.PackKits, kit)
		generatedKitDirs = append(generatedKitDirs, kit)
	}
	if o.Models, err = callableRuntimeModels(cfg); err != nil {
		return fmt.Errorf("inference models: %w", err)
	}
	if kit, cerr := synthesizePersonalContextKit(); cerr != nil {
		return fmt.Errorf("personal context: %w", cerr)
	} else if kit != "" {
		o.PackKits = append(o.PackKits, kit)
		generatedKitDirs = append(generatedKitDirs, kit)
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
	// Same local-image preflight as `pix run`: a task pins --template to the
	// local-<ts> tag, so a stale tag (pruned template) would make sbx pull a
	// never-published image and stall on a prompt. Refuse fast with `make load`.
	if o.LocalImageTag != "" && len(o.Kits) == 0 && o.LocalKit != "" {
		if !localImageLoaded(defaultShellEnv(), o.LocalImageTag) {
			return fmt.Errorf("local image %s:%s is not loaded in sbx (it's a local build, never published).\n"+
				"Load this build first, from your pix checkout:  make load", dockerImageRepo, o.LocalImageTag)
		}
	}

	// Resolve any pre-existing sandbox of the derived name atomically before
	// launching into it (R5-1): a stopped one is removed with `sbx rm` (no -f),
	// a running or indeterminate one is refused. Never force-kill without --force.
	if err := prepareTaskLaunchSandbox(defaultShellEnv(), o.Name); err != nil {
		return err
	}

	wireKnowledgeScope(cfg, o.Workspace, defaultKnowledgeRPC())

	// Mirror run.go's pack-context writes (packs-v2 Phase 1 gap): applyPackToLaunch
	// above only updates cfg/o with the pack's overrides, it does not write the
	// per-launch workspace files that carry that context INTO the sandbox. Without
	// these a task sandbox silently loses the active pack's memory scope, its
	// ollama-bridge model, and the stale-pack marker run.go relies on. A task is
	// always a fresh create, so writeMemoryScope and writeSandboxPackMarker run
	// unconditionally (no willCreate/definitelyCreating gating needed — that only
	// exists in run.go to distinguish create from re-attach, and a task never
	// re-attaches).
	writePackContextFiles(cfg, o, effectivePack)
	writeSandboxPackMarker(o.Workspace, effectivePack)

	// Resolve every configured MCP server to attach at create (--static-mcp).
	// S01: all of them preload. A task is always a fresh create, so it's always
	// needed.
	o.StaticMCP = allPreloadedMCP(append(append([]string(nil), cfg.MCP...), o.MCP...))

	args := buildSbxArgs(cfg, o, version)
	if os.Getenv("PIX_DEBUG") != "" {
		fmt.Fprintln(os.Stderr, "+ sbx "+strings.Join(args, " "))
	}
	cmd := exec.Command("sbx", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	// A task is ALWAYS a fresh create, so it runs the same corrected create
	// lifecycle as `pix run` (run.go): pre-create receipt clear, Start,
	// creation-evidence poll, mid-session receipt commit for o.StaticMCP, Wait.
	// A *receiptRecordError here reaches `task new`'s error report as-is — the
	// sandbox probe there reads running/stopped, so the clone is kept and the
	// honest "created but unrecorded" message is printed, never a rollback.
	return execSbxRunAndRecordCreate(cmd, true, o.Name, canonicalWorkspacePath(o.Workspace), o.StaticMCP)
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
	// Unreadable marks a row built from metadata that could not be read or
	// trusted (AC-P0-009): the row is degraded, never omitted. Omission is
	// unrecoverable (the task silently vanishes from `task ls`); a visibly
	// broken row is recoverable (the operator knows the file, can inspect or
	// remove it).
	Unreadable bool `json:"unreadable,omitempty"`
}

// taskMetaUnreadableStatus is the fixed marker rendered in place of a normal
// status/git line when a task's meta.json could not be read or trusted. It is
// asserted verbatim by tests, so keep it a literal constant rather than a
// format string that could drift.
const taskMetaUnreadableStatus = "\u2298 unreadable metadata"

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
			fmt.Fprintf(os.Stderr, "pix task ls: unknown argument %q\n\n%s", a, taskUsage)
			os.Exit(2)
		}
	}

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task ls: not a git repository: %v\n", err)
		os.Exit(1)
	}
	repokey := taskRepoKey(mainroot)

	var rows []taskListRow
	gcEligible := 0
	for _, lay := range existingTaskLayouts(mainroot) {
		metaDir := filepath.Join(taskStateRoot(), lay.dir, "meta")
		entries, _ := os.ReadDir(metaDir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			fileBase := strings.TrimSuffix(e.Name(), ".json")
			m, err := readTaskMeta(filepath.Join(metaDir, e.Name()))
			if err == nil {
				// Never trust stored branch/sandbox/mainroot for building refs or argv;
				// re-derive them from the validated name + trusted layout.
				m, err = hardenTaskMeta(m, mainroot, repokey, lay.legacy, fileBase)
			}
			if err != nil {
				// AC-P0-009: never hide a row. Genuinely unreadable or untrustworthy
				// metadata still gets a place in the list, degraded rather than
				// silently skipped — the operator needs to know the file exists and
				// is broken, not have it vanish. The full error still goes to stderr
				// under PIX_DEBUG so it stays diagnosable without spamming
				// normal output.
				if os.Getenv("PIX_DEBUG") != "" {
					fmt.Fprintf(os.Stderr, "pix task ls: unreadable metadata %s: %v\n", e.Name(), err)
				}
				rows = append(rows, taskListRow{
					Name:       fileBase,
					Status:     taskMetaUnreadableStatus,
					Unreadable: true,
				})
				continue
			}
			co, _ := taskPaths(lay.dir, sanitizeTaskName(m.Name))
			st := gatherTaskState(env, m, co)
			if st.sandbox != sbxRunning && !st.dirty && !st.unknown && st.unrec == 0 &&
				taskAge(m, co) >= taskGCDefaultDays*24*time.Hour {
				gcEligible++
			}
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
		fmt.Println("Next: `pix task new <name>` to start one.")
		return
	}
	fmt.Printf("Tasks for %s (%s):\n", taskRepoLabel(mainroot), mainroot)
	fmt.Printf("%-20s %-24s %-10s %s\n", "NAME", "BRANCH", "SANDBOX", "GIT")
	for _, r := range rows {
		if r.Unreadable {
			fmt.Printf("%-20s %-24s %-10s %s\n", r.Name, "-", "-", taskMetaUnreadableStatus)
			continue
		}
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
	if gcEligible > 0 {
		fmt.Printf("%s clean and older than %dd; `pix task gc` to prune.\n",
			plural(gcEligible, "task"), taskGCDefaultDays)
	}
	fmt.Println("Next: `pix task rm <name>` to tear one down (guarded).")
}

// ---------------------------------------------------------------------------
// task path
// ---------------------------------------------------------------------------

// runTaskPath prints ONLY the task's checkout dir to stdout so it composes in a
// shell: `cd "$(pix task foo path)"`. Everything else (errors, hints) goes
// to stderr, and a missing task is exit 1 with nothing on stdout. It is
// read-only: no lock, no meta hardening beyond confirming the task exists.
func runTaskPath(env shellEnv, argv []string) {
	if wantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	if len(argv) != 1 || strings.HasPrefix(argv[0], "-") {
		fmt.Fprintf(os.Stderr, "pix task path: expected exactly one task name\n\n%s", taskUsage)
		os.Exit(2)
	}
	name := argv[0]

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task path: not a git repository: %v\n", err)
		os.Exit(1)
	}
	sane := sanitizeTaskName(name)
	lay, found, ambiguous := findTaskLayout(mainroot, sane)
	if ambiguous {
		fmt.Fprintf(os.Stderr, "pix task path: %q exists in BOTH the new and legacy task layout for this repo; refusing to guess.\n", name)
		os.Exit(1)
	}
	if !found {
		fmt.Fprintf(os.Stderr, "pix task path: no such task %q for this repo\n", name)
		os.Exit(1)
	}
	co, _ := taskPaths(lay.dir, sane)
	fmt.Println(co)
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
		fmt.Fprintf(os.Stderr, "pix task rm: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: not a git repository: %v\n", err)
		os.Exit(1)
	}
	repokey := taskRepoKey(mainroot)
	sane := sanitizeTaskName(name)
	lay, found, ambiguous := findTaskLayout(mainroot, sane)
	if ambiguous {
		fmt.Fprintf(os.Stderr, "pix task rm: %q exists in BOTH the new and legacy task layout for this repo; refusing to guess.\n", name)
		fmt.Fprintf(os.Stderr, "Inspect %s and %s under the tasks state dir and remove one by hand.\n",
			taskRepoDir(mainroot), taskRepoKey(mainroot))
		os.Exit(1)
	}
	_ = found // a not-found task falls through to the readTaskMeta "no such task" path
	repoDir := lay.dir
	co, metaPath := taskPaths(repoDir, sane)

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
			fmt.Fprintf(os.Stderr, "pix task rm: no such task %q for this repo\n", name)
			exitCode = 1
			return nil
		}
		// Never trust stored branch/sandbox/mainroot for building refs or the
		// `sbx rm` argv; re-derive them from the validated name. A meta that fails
		// validation is tampered or corrupt, so refuse rather than act on it.
		meta, err = hardenTaskMeta(meta, mainroot, repokey, lay.legacy, sane)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pix task rm: refusing to remove %q: %v\n", name, err)
			fmt.Fprintln(os.Stderr, "The task metadata looks tampered or corrupt; inspect it under the tasks state dir before retrying.")
			exitCode = 1
			return nil
		}

		st := gatherTaskState(env, meta, co)
		if msg, ok := taskRemoveGuard(st, force); !ok {
			fmt.Fprintf(os.Stderr, "pix task rm: refusing to remove %q: %s.\n", name, msg)
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
		recovered := "refs/pix/recovered/" + sane
		if rc := executeTaskTeardown(env, os.Stderr, meta, co, name, recovered, force, st); rc != 0 {
			exitCode = rc
			return nil
		}
		// b2. ALWAYS-ON harvest, AFTER the sandbox is gone (so no live session can
		// write into the clone mid-enumeration) but BEFORE the checkout is deleted.
		// FAIL-CLOSED: if enumeration or any copy fails, KEEP the clone + metadata and
		// abort rather than delete the only copy of un-git'd docs. This mirrors the ref
		// snapshot (committed work); harvest rescues the uncommitted file half, which
		// --force would otherwise destroy.
		if _, herr := harvestArtifacts(env, os.Stderr, meta, repoDir, co, name); herr != nil {
			fmt.Fprintf(os.Stderr, "pix task rm: sandbox %s was removed, but the uncommitted docs could not be safely harvested: %v\n", meta.Sandbox, herr)
			fmt.Fprintf(os.Stderr, "Leaving the clone intact at %s so nothing is lost. Copy the docs out, then re-run `pix task rm %s` (the sandbox is already gone).\n", co, name)
			exitCode = 1
			return nil
		}
		// c. remove checkout + metadata. The main repo's branches are never touched.
		// If either removal fails, name what remains and exit non-zero rather than
		// print a false "Removed" claim.
		if err := removeTaskArtifacts(co, metaPath, os.RemoveAll, os.Remove); err != nil {
			fmt.Fprintf(os.Stderr, "pix task rm: sandbox %s was torn down but %v\n", meta.Sandbox, err)
			fmt.Fprintln(os.Stderr, "Remove the leftover state by hand before reusing this task name.")
			exitCode = 1
			return nil
		}

		fmt.Printf("Removed task %q (sandbox %s).\n", name, meta.Sandbox)
		fmt.Printf("Next: recovered commits (if any) are at %s in %s.\n", recovered, meta.Mainroot)
		return nil
	})
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "pix task rm: %v\n", lockErr)
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

// ---------------------------------------------------------------------------
// Artifacts (harvest): rescue uncommitted docs so `rm --force`/`gc` never
// silently destroy un-git'd work. Destination is XDG_DATA_HOME, NOT the task
// STATE dir and NOT the knowledge bundle: harvested docs are user work product
// that must outlive `state reset`/`uninstall`, and shared-truth promotion into
// OKF stays a deliberate `enrich` act.
// ---------------------------------------------------------------------------

// taskArtifactRoot is the durable base dir for harvested artifacts:
// $XDG_DATA_HOME/pix/artifacts (default ~/.local/share/pix/artifacts).
// Deliberately under DATA_HOME, not the STATE tree the clones live in, so
// `state reset`/`uninstall` never reach it.
func taskArtifactRoot() string {
	if x := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); x != "" {
		return filepath.Join(x, "pix", "artifacts")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "pix", "artifacts")
}

// artifactExts is the default set of doc file extensions harvest rescues.
// Overridable via PIX_TASK_ARTIFACT_EXTS (comma-separated, with or without
// the leading dot).
var artifactExts = []string{".md", ".markdown", ".prd", ".txt", ".rst", ".adoc"}

// artifactDirs is the default set of path prefixes whose files are always
// rescued regardless of extension (a task's scratch docs live here).
var artifactDirs = []string{"docs/", "notes/", ".pix/"}

// artifactSkipSegments are path segments that are never harvested even when they
// contain matching files (dependency + build trees produce huge ignored dirs).
var artifactSkipSegments = map[string]bool{
	"node_modules": true, ".git": true, "vendor": true,
	"dist": true, "build": true, "target": true, ".venv": true,
}

// isArtifactPath decides whether a repo-relative path is a doc artifact worth
// rescuing: not inside a skip segment, and either under a known doc dir or
// carrying a doc extension.
func isArtifactPath(rel string) bool {
	rel = filepath.ToSlash(rel)
	for _, seg := range strings.Split(rel, "/") {
		if artifactSkipSegments[seg] {
			return false
		}
	}
	for _, d := range artifactDirs {
		if strings.HasPrefix(rel, d) {
			return true
		}
	}
	ext := strings.ToLower(filepath.Ext(rel))
	for _, e := range artifactExts {
		if ext == e {
			return true
		}
	}
	return false
}

func init() {
	if v := strings.TrimSpace(os.Getenv("PIX_TASK_ARTIFACT_EXTS")); v != "" {
		var exts []string
		for _, e := range strings.Split(v, ",") {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			exts = append(exts, strings.ToLower(e))
		}
		if len(exts) > 0 {
			artifactExts = exts
		}
	}
}

type harvestFile struct {
	Path   string `json:"path"`   // repo-relative
	Status string `json:"status"` // git porcelain code: "??" untracked, "!!" ignored
}

type harvestManifest struct {
	Task        string        `json:"task"`
	Repo        string        `json:"repo"`
	Mainroot    string        `json:"mainroot"`
	Branch      string        `json:"branch"`
	Base        string        `json:"base"`
	Profile     string        `json:"profile"`
	Origin      string        `json:"origin"`
	HarvestedAt string        `json:"harvested_at"`
	Files       []harvestFile `json:"files"`
}

// listHarvestCandidates returns the untracked + ignored files in co that pass
// isArtifactPath, via `git status --porcelain=v1 -z` (NUL-delimited). The -z
// form is the ONLY reliable way to read pathnames: default porcelain C-quotes
// paths containing spaces, tabs, newlines, quotes, or non-ASCII, and
// core.quotePath=false does NOT disable all of that. NUL records carry raw
// bytes, so nothing is mis-parsed and silently dropped (which, on the rm path,
// would delete an un-git'd doc the filter never saw). A git failure returns the
// error so the caller fails CLOSED (rm/gc leave the checkout intact).
func listHarvestCandidates(env shellEnv, co string) ([]harvestFile, error) {
	seen := map[string]bool{}
	var files []harvestFile
	add := func(rel, status string) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		files = append(files, harvestFile{Path: rel, Status: status})
	}

	// 1. Untracked + ignored docs.
	out, err := env.Run("git", "-C", co,
		"status", "--porcelain=v1", "-z", "--ignored", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(out))
	}
	for _, rec := range strings.Split(out, "\x00") {
		if len(rec) < 4 {
			continue
		}
		code := rec[:2]
		if code != "??" && code != "!!" {
			continue
		}
		rel := rec[3:]
		if !safeRelPath(rel) {
			continue // never let a ".."/absolute path escape the dest tree
		}
		// git reports an untracked/ignored DIR as a single entry with a trailing
		// slash; expand it to the matching files beneath it.
		if strings.HasSuffix(rel, "/") {
			sub, derr := expandHarvestDir(co, rel, code)
			if derr != nil {
				return nil, derr // fail CLOSED: an unreadable ignored dir must not read as "nothing"
			}
			for _, f := range sub {
				add(f.Path, f.Status)
			}
			continue
		}
		if isArtifactPath(rel) {
			add(rel, code)
		}
	}

	// 2. TRACKED docs modified vs HEAD (staged or unstaged). The ref snapshot only
	// captures committed bytes, so a committed-then-edited doc dropped under
	// --force would lose its working-tree changes. `git diff --name-only -z HEAD`
	// lists changed tracked paths (renames report the new name); we keep only ones
	// that still exist on disk (skip deletions) and pass the doc filter.
	dout, derr := env.Run("git", "-C", co, "diff", "--name-only", "-z", "HEAD")
	if derr != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(dout))
	}
	for _, rel := range strings.Split(dout, "\x00") {
		if rel == "" || !safeRelPath(rel) || !isArtifactPath(rel) {
			continue
		}
		fi, err := os.Lstat(filepath.Join(co, filepath.FromSlash(rel)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // a deleted tracked doc has nothing to rescue
			}
			return nil, fmt.Errorf("stat %s: %w", rel, err) // fail CLOSED on a real stat error
		}
		if fi.Mode().IsRegular() {
			add(rel, "M")
		}
	}
	return files, nil
}

// safeRelPath reports whether a git-reported path is a plain repo-relative path
// that cannot escape a destination tree: not absolute, no ".." segment. Defense
// in depth — git never emits such paths, but harvest writes them under a dest
// dir, so a malformed one must never traverse out.
func safeRelPath(rel string) bool {
	if rel == "" || filepath.IsAbs(rel) {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// expandHarvestDir walks a reported untracked/ignored directory (rel ends in
// "/") and returns its files that pass isArtifactPath. A skip segment anywhere in
// the dir prunes the walk cheaply. It PROPAGATES a real walk/Rel error (returns
// it) so the caller fails closed: an unreadable ignored dir must never silently
// read as "no docs here" and green-light teardown.
func expandHarvestDir(co, rel, code string) ([]harvestFile, error) {
	for _, seg := range strings.Split(strings.Trim(filepath.ToSlash(rel), "/"), "/") {
		if artifactSkipSegments[seg] {
			return nil, nil
		}
	}
	var files []harvestFile
	root := filepath.Join(co, filepath.FromSlash(rel))
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err // propagate: a permission/IO error must fail the harvest
		}
		if d.IsDir() {
			if artifactSkipSegments[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		r, err := filepath.Rel(co, p)
		if err != nil {
			return err
		}
		r = filepath.ToSlash(r)
		if isArtifactPath(r) {
			files = append(files, harvestFile{Path: r, Status: code})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan ignored dir %s: %w", rel, err)
	}
	return files, nil
}

// harvestArtifacts copies the matching untracked/ignored/modified docs from co
// into a fresh timestamped dir under taskArtifactRoot()/<repoDir>/<name>/<ts>/,
// writes a manifest.json beside them, and returns the destination dir ("" when
// nothing matched). It is idempotent: each call writes a NEW timestamped dir, so
// re-running never clobbers a prior capture. It returns a NON-NIL error when
// enumeration, any copy, or the manifest write fails, so rm/gc callers fail
// CLOSED (keep the clone) rather than delete the only copy of un-git'd docs.
func harvestArtifacts(env shellEnv, w io.Writer, meta taskMeta, repoDir, co, name string) (string, error) {
	files, err := listHarvestCandidates(env, co)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", nil
	}
	// repoDir is already path-safe (label+repokey); name is raw user input, so it
	// is sanitized. pruneArtifacts walks the same taskArtifactRoot()/<repoDir> tree.
	parent := filepath.Join(taskArtifactRoot(), repoDir, sanitizeTaskName(name))
	dest, err := freshSnapshotDir(parent)
	if err != nil {
		return "", fmt.Errorf("create artifacts dir: %w", err)
	}
	var copied []harvestFile
	var copyErrs []string
	for _, f := range files {
		src := filepath.Join(co, filepath.FromSlash(f.Path))
		dst := filepath.Join(dest, filepath.FromSlash(f.Path))
		ok, err := copyFilePreserve(src, dst)
		if err != nil {
			copyErrs = append(copyErrs, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		if ok {
			copied = append(copied, f) // only count files actually written
		}
	}
	man := harvestManifest{
		Task: name, Repo: meta.Repo, Mainroot: meta.Mainroot, Branch: meta.Branch,
		Base: meta.Base, Profile: meta.Profile, Origin: stripURLUserinfo(meta.Origin),
		HarvestedAt: time.Now().UTC().Format(time.RFC3339), Files: copied,
	}
	if b, err := json.MarshalIndent(man, "", "  "); err == nil {
		if werr := os.WriteFile(filepath.Join(dest, "manifest.json"), append(b, '\n'), 0o600); werr != nil {
			// A manifest we could not write means the capture is not trustworthy;
			// surface it as a harvest failure so the caller fails closed.
			copyErrs = append(copyErrs, fmt.Sprintf("manifest: %v", werr))
		}
	}
	if len(copyErrs) > 0 {
		return dest, fmt.Errorf("some artifacts could not be copied: %s", strings.Join(copyErrs, "; "))
	}
	if len(copied) > 0 {
		fmt.Fprintf(w, "pix: harvested %s to %s\n", plural(len(copied), "doc"), dest)
	}
	return dest, nil
}

// freshSnapshotDir creates a NEW timestamped snapshot dir under parent, never
// reusing an existing one (the fresh-capture guarantee). The base timestamp has
// 1s precision, so it retries with a -<n> suffix on collision using O_EXCL-style
// Mkdir (fails on an existing dir), guaranteeing two harvests in the same second
// get distinct dirs.
func freshSnapshotDir(parent string) (string, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", err
	}
	base := time.Now().UTC().Format("20060102T150405Z")
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		dest := filepath.Join(parent, name)
		err := os.Mkdir(dest, 0o700)
		if err == nil {
			return dest, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	}
	return "", fmt.Errorf("could not allocate a fresh snapshot dir under %s", parent)
}

// copyFilePreserve copies src to dst (creating parent dirs), preserving the file
// mode. It returns copied=false (no error) for a non-regular source (symlink or
// special) so the caller does not record it as harvested when nothing was
// written. A real IO error is returned so the caller can fail closed.
func copyFilePreserve(src, dst string) (copied bool, err error) {
	fi, err := os.Lstat(src)
	if err != nil {
		return false, err
	}
	if !fi.Mode().IsRegular() {
		return false, nil // skip symlinks/specials; not a doc to rescue
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return false, err
	}
	// Alias guard: refuse if dst resolves to the SAME file as src (a --to dir with
	// a symlink like docs -> <checkout>/docs makes dst alias the source). Lstat
	// follows intermediate components, so this catches the descendant-symlink case;
	// overwriting src with a copy of itself is refused rather than risked.
	if dfi, derr := os.Lstat(dst); derr == nil && os.SameFile(fi, dfi) {
		return false, fmt.Errorf("destination %s aliases the source file; refusing to overwrite it", dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return false, err
	}
	defer in.Close()
	// Write to a temp file in the SAME dir, then atomically rename over dst. Rename
	// replaces the NAME (never follows/truncates a symlink at dst) and is atomic, so
	// a copy/close error never leaves a partial dst OR destroys a prior good copy
	// (the temp is removed and dst is untouched).
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".harvest-*")
	if err != nil {
		return false, err
	}
	tmpName := tmp.Name()
	cleanup := func(e error) (bool, error) {
		tmp.Close()
		_ = os.Remove(tmpName)
		return false, e
	}
	if _, err := io.Copy(tmp, in); err != nil {
		return cleanup(err)
	}
	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return false, err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		_ = os.Remove(tmpName)
		return false, err
	}
	return true, nil
}

// runTaskHarvest is `task harvest <name> [--to DIR]`: the deliberate, re-runnable
// capture. It reads + hardens the meta like rm/ls, then harvests. No sandbox or
// guard interaction — it only ever COPIES from the host clone, so it is safe to
// run mid-task, repeatedly.
func runTaskHarvest(env shellEnv, argv []string) {
	if wantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	name, to, err := parseTaskHarvestArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task harvest: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}
	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task harvest: not a git repository: %v\n", err)
		os.Exit(1)
	}
	repokey := taskRepoKey(mainroot)
	sane := sanitizeTaskName(name)
	lay, _, ambiguous := findTaskLayout(mainroot, sane)
	if ambiguous {
		fmt.Fprintf(os.Stderr, "pix task harvest: %q exists in both the new and legacy layout; refusing to guess.\n", name)
		os.Exit(1)
	}
	repoDir := lay.dir
	co, metaPath := taskPaths(repoDir, sane)

	// Under the per-task lock so a concurrent rm/new cannot delete or recreate the
	// checkout mid-copy (a partial or cross-generation capture).
	exitCode := 0
	lockErr := withTaskLock(repokey, sane, func() error {
		meta, err := readTaskMeta(metaPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pix task harvest: no such task %q for this repo\n", name)
			exitCode = 1
			return nil
		}
		meta, err = hardenTaskMeta(meta, mainroot, repokey, lay.legacy, sane)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pix task harvest: refusing %q: %v\n", name, err)
			exitCode = 1
			return nil
		}
		if to != "" {
			exitCode = harvestToDir(env, co, to, name)
			return nil
		}
		dest, herr := harvestArtifacts(env, os.Stdout, meta, repoDir, co, name)
		if herr != nil {
			fmt.Fprintf(os.Stderr, "pix task harvest: %v\n", herr)
			exitCode = 1
			return nil
		}
		if dest == "" {
			fmt.Println("No uncommitted doc artifacts to harvest.")
		}
		return nil
	})
	if lockErr != nil {
		fmt.Fprintf(os.Stderr, "pix task harvest: %v\n", lockErr)
		os.Exit(1)
	}
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// harvestToDir copies the harvest candidates to an explicit user-chosen dir. It
// REFUSES a destination equal to or nested under the checkout (source ==
// destination would open a file O_TRUNC on itself, destroying the original), and
// it accumulates copy failures and reports a non-zero exit code rather than a
// false success. Returns the exit code (0 ok, 1 error).
func harvestToDir(env shellEnv, co, to, name string) int {
	coAbs, err := absResolve(co)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task harvest: %v\n", err)
		return 1
	}
	toAbs, err := filepath.Abs(to)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task harvest: %v\n", err)
		return 1
	}
	// Resolve symlinks so a symlinked dest can't alias the checkout. This has to
	// canonicalize a --to that does NOT exist yet too: coAbs is EvalSymlinks'd
	// above, so leaving a not-yet-created dest merely Abs+Clean'd compares
	// /var/…/co/sub against /private/var/…/co and the nesting check fails OPEN —
	// harvest then copies into the checkout it was meant to refuse. Any symlinked
	// ancestor does this (macOS /var, a symlinked $HOME, /home -> /mnt/home), not
	// just the dest itself.
	toAbs = resolveThroughMissing(toAbs)
	if toAbs == coAbs || strings.HasPrefix(toAbs+string(os.PathSeparator), coAbs+string(os.PathSeparator)) {
		fmt.Fprintf(os.Stderr, "pix task harvest: --to %q is the checkout itself (or nested inside it); choose a dir outside the clone.\n", to)
		return 1
	}
	files, lerr := listHarvestCandidates(env, co)
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "pix task harvest: %v\n", lerr)
		return 1
	}
	if len(files) == 0 {
		fmt.Println("No uncommitted doc artifacts to harvest.")
		return 0
	}
	var errs []string
	copied := 0
	for _, f := range files {
		ok, err := copyFilePreserve(filepath.Join(co, filepath.FromSlash(f.Path)), filepath.Join(toAbs, filepath.FromSlash(f.Path)))
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f.Path, err))
			continue
		}
		if ok {
			copied++
		}
	}
	if len(errs) > 0 {
		fmt.Fprintf(os.Stderr, "pix task harvest: %d of %d docs could not be copied: %s\n", len(errs), len(files), strings.Join(errs, "; "))
		return 1
	}
	fmt.Printf("Harvested %s to %s\n", plural(copied, "doc"), toAbs)
	return 0
}

// resolveThroughMissing canonicalizes a path that may not exist yet, so it is
// comparable byte-for-byte with an absResolve'd sibling. EvalSymlinks fails
// outright on a missing path, so walk up to the deepest ancestor that DOES
// exist, resolve that, and re-append the segments below it. A path with no
// resolvable ancestor at all falls back to its cleaned absolute form.
func resolveThroughMissing(abs string) string {
	cur := filepath.Clean(abs)
	rest := ""
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Clean(abs)
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// absResolve returns the absolute, symlink-resolved path.
func absResolve(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(a); err == nil {
		return r, nil
	}
	return a, nil
}

// parseTaskHarvestArgs parses `harvest <name> [--to DIR]`.
func parseTaskHarvestArgs(argv []string) (name, to string, err error) {
	nameSet := false
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--to" || strings.HasPrefix(a, "--to="):
			if eq := strings.IndexByte(a, '='); eq >= 0 {
				to = a[eq+1:]
			} else {
				if i+1 >= len(argv) {
					return "", "", fmt.Errorf("flag --to needs a value")
				}
				i++
				to = argv[i]
			}
		case strings.HasPrefix(a, "-"):
			return "", "", fmt.Errorf("unknown flag %q", a)
		default:
			if nameSet {
				return "", "", fmt.Errorf("unexpected extra argument %q", a)
			}
			name = a
			nameSet = true
		}
	}
	if !nameSet || strings.TrimSpace(name) == "" {
		return "", "", fmt.Errorf("a task name is required")
	}
	return name, to, nil
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

// ---------------------------------------------------------------------------
// task gc: prune clean, over-age tasks + retain artifact snapshots.
// ---------------------------------------------------------------------------

const (
	// taskGCDefaultDays is the default age (in days) a task must exceed before gc
	// will prune it. Age = now - max(meta.Created, mtime(co)).
	taskGCDefaultDays = 7
	// taskArtifactDefaultDays is the default retention for harvested artifact
	// snapshots. Pruning these is pure age-gated deletion (the rescue copy has
	// already served its purpose), never gated by the clone guard.
	taskArtifactDefaultDays = 30
)

type taskGcOpts struct {
	days         int
	artifactDays int
	dryRun       bool
	noHarvest    bool
}

// taskAge is a task's age for gc: now - max(meta.Created, mtime(co)). Created
// alone is a bad proxy (a weeks-old task you use daily looks stale); the checkout
// mtime reflects real activity. A task with neither timestamp available returns 0
// (never over-age), so a broken meta is never auto-pruned.
func taskAge(meta taskMeta, co string) time.Duration {
	var newest time.Time
	bump := func(t time.Time) {
		if t.After(newest) {
			newest = t
		}
	}
	if strings.TrimSpace(meta.Created) != "" {
		if t, err := time.Parse(time.RFC3339, meta.Created); err == nil {
			bump(t)
		}
	}
	// The checkout DIRECTORY mtime does not change when a file inside it is
	// edited, so it is a weak activity signal alone. Also consult git's own
	// activity markers: the reflog (commits/checkouts) and the index (staging),
	// plus the checkout root, and take the most recent. A task committed or
	// staged recently then reads as active, not stale.
	for _, p := range []string{
		co,
		filepath.Join(co, ".git", "logs", "HEAD"),
		filepath.Join(co, ".git", "index"),
	} {
		if fi, err := os.Stat(p); err == nil {
			bump(fi.ModTime())
		}
	}
	if newest.IsZero() {
		return 0
	}
	return time.Since(newest)
}

// runTaskGc prunes clean, over-age tasks for the current repo. It reuses the
// EXACT rm machinery per candidate (gatherTaskState + taskRemoveGuard +
// harvest + executeTaskTeardown + removeTaskArtifacts) under the per-task lock,
// so a task with uncommitted/unpushed work is skipped and reported, never
// force-dropped (there is no --force on gc, by design). After the task sweep it
// prunes harvested artifact snapshots older than --artifact-days.
func runTaskGc(env shellEnv, argv []string) {
	if wantsHelp(argv) {
		fmt.Print(taskUsage)
		return
	}
	opts, err := parseTaskGcArgs(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task gc: %v\n\n%s", err, taskUsage)
		os.Exit(2)
	}

	cwd, _ := os.Getwd()
	mainroot, err := resolveMainroot(env, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix task gc: not a git repository: %v\n", err)
		os.Exit(1)
	}
	repokey := taskRepoKey(mainroot)
	maxAge := time.Duration(opts.days) * 24 * time.Hour
	// skipped = normal guard refusals (dirty/unpushed/running) — expected, exit 0.
	// failed  = OPERATIONAL failures (corrupt meta, harvest/teardown/remove error)
	// — a safety operation did not complete, so gc must exit non-zero so automation
	// never reads a failed prune as success.
	removed, skipped, wouldRemove, failed := 0, 0, 0, 0

	// Discover candidate (layout, name) pairs across BOTH the new and legacy
	// layouts. The scan is only for DISCOVERY; the authoritative read + harden +
	// age + gather + guard all happen INSIDE the per-task lock below, so a task
	// removed and recreated while gc waited for the lock is never torn down on a
	// stale decision (the pre-lock TOCTOU).
	type cand struct {
		lay      taskLayout
		fileBase string
	}
	var cands []cand
	for _, lay := range existingTaskLayouts(mainroot) {
		entries, _ := os.ReadDir(filepath.Join(taskStateRoot(), lay.dir, "meta"))
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			cands = append(cands, cand{lay, strings.TrimSuffix(e.Name(), ".json")})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].fileBase < cands[j].fileBase })

	for _, c := range cands {
		repoDir := c.lay.dir
		fileBase := c.fileBase
		co, metaPath := taskPaths(repoDir, fileBase)
		lockErr := withTaskLock(repokey, fileBase, func() error {
			// Re-read + re-harden + re-age UNDER the lock: the file may have been
			// removed/recreated since discovery.
			m, err := readTaskMeta(metaPath)
			if err != nil {
				return nil // gone since discovery; nothing to do
			}
			m, err = hardenTaskMeta(m, mainroot, repokey, c.lay.legacy, fileBase)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gc: skipping %s: %v\n", fileBase, err)
				failed++
				return nil
			}
			age := taskAge(m, co)
			if age < maxAge {
				return nil // not a candidate under the lock either
			}
			name := m.Name
			st := gatherTaskState(env, m, co)
			if msg, ok := taskRemoveGuard(st, false); !ok {
				fmt.Printf("skip %-20s %s\n", name, msg)
				skipped++
				return nil
			}
			if opts.dryRun {
				fmt.Printf("would remove %-20s (age %dd)\n", name, int(age.Hours()/24))
				wouldRemove++
				return nil
			}
			recovered := "refs/pix/recovered/" + fileBase
			if rc := executeTaskTeardown(env, os.Stderr, m, co, name, recovered, false, st); rc != 0 {
				failed++
				return nil
			}
			// Harvest AFTER teardown (sandbox gone), fail-closed (unless --no-harvest):
			// a harvest error keeps the clone + metadata rather than deleting un-git'd
			// docs. gc only reaches here on a guard-clean task, so there is normally
			// nothing to harvest; this is the belt-and-braces path.
			if !opts.noHarvest {
				if _, herr := harvestArtifacts(env, os.Stdout, m, repoDir, co, name); herr != nil {
					fmt.Fprintf(os.Stderr, "gc: %-20s sandbox removed but harvest failed; keeping clone at %s: %v\n", name, co, herr)
					failed++
					return nil
				}
			}
			if err := removeTaskArtifacts(co, metaPath, os.RemoveAll, os.Remove); err != nil {
				fmt.Fprintf(os.Stderr, "gc: %v\n", err)
				failed++
				return nil
			}
			fmt.Printf("removed %-20s (age %dd, sandbox %s)\n", name, int(age.Hours()/24), m.Sandbox)
			removed++
			return nil
		})
		if lockErr != nil {
			fmt.Fprintf(os.Stderr, "gc: %s: %v\n", fileBase, lockErr)
			failed++
		}
	}

	prunedArtifacts := pruneArtifacts(taskRepoDir(mainroot), opts.artifactDays, opts.dryRun) +
		pruneArtifacts(taskRepoKey(mainroot), opts.artifactDays, opts.dryRun)

	if opts.dryRun {
		fmt.Printf("gc (dry run): %d would be removed, %d skipped, %d failed, %s would be pruned.\n",
			wouldRemove, skipped, failed, plural(prunedArtifacts, "artifact snapshot"))
	} else {
		fmt.Printf("gc: %d removed, %d skipped, %d failed, %s pruned.\n",
			removed, skipped, failed, plural(prunedArtifacts, "artifact snapshot"))
	}
	// A guard skip is normal (exit 0); an operational failure is not — exit non-zero
	// so automation never treats a failed safety operation as success.
	if failed > 0 {
		os.Exit(1)
	}
}

// pruneArtifacts deletes harvested artifact snapshot dirs older than days under
// taskArtifactRoot()/<repoDir>/<task>/<ts>/, by the snapshot dir's mtime. Pure
// age-gated deletion (the snapshot was already the rescue copy). Returns the
// count pruned (or that WOULD be pruned under dryRun). days <= 0 disables it.
func pruneArtifacts(repoDir string, days int, dryRun bool) int {
	if days <= 0 {
		return 0
	}
	base := filepath.Join(taskArtifactRoot(), repoDir)
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	pruned := 0
	tasks, err := os.ReadDir(base)
	if err != nil {
		return 0
	}
	for _, t := range tasks {
		if !t.IsDir() {
			continue
		}
		taskDir := filepath.Join(base, t.Name())
		snaps, err := os.ReadDir(taskDir)
		if err != nil {
			continue
		}
		for _, s := range snaps {
			if !s.IsDir() {
				continue
			}
			info, err := s.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			snapPath := filepath.Join(taskDir, s.Name())
			if dryRun {
				pruned++
				continue
			}
			if err := os.RemoveAll(snapPath); err == nil {
				pruned++
			}
		}
	}
	return pruned
}

// parseTaskGcArgs parses `gc [--days N] [--dry-run] [--no-harvest] [--artifact-days N]`.
func parseTaskGcArgs(argv []string) (taskGcOpts, error) {
	o := taskGcOpts{days: taskGCDefaultDays, artifactDays: taskArtifactDefaultDays}
	intVal := func(a string, i *int) (string, error) {
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			return a[eq+1:], nil
		}
		if *i+1 >= len(argv) {
			return "", fmt.Errorf("flag %s needs a value", a)
		}
		*i++
		return argv[*i], nil
	}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		n := a
		if eq := strings.IndexByte(a, '='); eq >= 0 {
			n = a[:eq]
		}
		switch n {
		case "--days":
			v, err := intVal(a, &i)
			if err != nil {
				return o, err
			}
			d, err := parseDays(v)
			if err != nil {
				return o, fmt.Errorf("--days %v", err)
			}
			o.days = d
		case "--artifact-days":
			v, err := intVal(a, &i)
			if err != nil {
				return o, err
			}
			d, err := parseDays(v)
			if err != nil {
				return o, fmt.Errorf("--artifact-days %v", err)
			}
			o.artifactDays = d
		case "--dry-run":
			o.dryRun = true
		case "--no-harvest":
			o.noHarvest = true
		default:
			return o, fmt.Errorf("unknown flag %q", a)
		}
	}
	return o, nil
}

// maxTaskGcDays caps --days / --artifact-days so `days * 24h` never overflows
// time.Duration (int64 ns) and wraps negative, which would invert the cutoff and
// prune everything instead of nothing.
const maxTaskGcDays = int((int64(1) << 62) / (24 * int64(time.Hour)))

func parseDays(v string) (int, error) {
	d, err := strconv.Atoi(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("must be a non-negative integer")
	}
	if d > maxTaskGcDays {
		return 0, fmt.Errorf("is too large (max %d)", maxTaskGcDays)
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// status summary (global, repo-agnostic): total task clones + artifacts size.
// ---------------------------------------------------------------------------

// taskStateSummary walks the task state + artifact roots to a global count of
// task clones and the on-disk size of harvested artifacts, so `pix status`
// can surface the pile without any per-repo git probing (that needs a cwd/repo).
// Best-effort: an unreadable tree contributes 0.
func taskStateSummary() (tasks int, artifactBytes int64) {
	repos, _ := os.ReadDir(taskStateRoot())
	for _, r := range repos {
		if !r.IsDir() {
			continue
		}
		metas, _ := os.ReadDir(filepath.Join(taskStateRoot(), r.Name(), "meta"))
		for _, m := range metas {
			if !m.IsDir() && strings.HasSuffix(m.Name(), ".json") {
				tasks++
			}
		}
	}
	_, artifactBytes = artifactDirSize(taskArtifactRoot())
	return tasks, artifactBytes
}

// artifactDirSize sums the file count + byte size under root (best-effort; an
// unreadable tree or a missing root contributes 0). Shared by the status summary
// and uninstall's "keeping artifacts" notice.
func artifactDirSize(root string) (files int, bytes int64) {
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		files++
		if info, err := d.Info(); err == nil {
			bytes += info.Size()
		}
		return nil
	})
	return files, bytes
}

// humanBytes renders a byte count as a short human string (B/KB/MB/GB).
// humanBytes now lives in monitor/tui (two packages need it; a second copy is
// how two renderers come to disagree about what "1.0MB" means).
func humanBytes(n int64) string { return tui.HumanBytes(n) }
