// task.go — sandbox-liveness primitives shared by `pix run` and `pix task`.
//
// Story06 simplification: everything about a task's git checkout (naming,
// metadata, the clone/worktree mechanism, the dirty/untracked/unrecoverable
// git-hygiene probe, the removal guard) moved OUT of this file and into the
// new L1 package "pix/host/workflow/task", which owns it without importing
// this package, workflow/launch/sandbox.go's `sbx` wrapper, or lease — see
// that package's doc comment and docs/design/worktree-tasks.md.
//
// What is left here is genuinely Story04/L4 territory: probing whether a
// named sandbox is running/stopped/absent, which `run.go` itself also needs
// (sandboxAppeared, the create-vs-reattach decision) — it is not
// task-specific despite the historical name. The `pix task` CLI dispatcher
// (new/run/ls/path/rm) now lives in cmd/pix/task_cmd.go, which composes the
// task package's checkout logic with this probe and with the EXISTING `pix
// run` path (no duplicated sandbox-launch/teardown code) and
// launch.RemovePixSandbox (sandbox.go) for teardown.
package launch

import (
	"path/filepath"
	"pix/host/hostenv"
	"pix/host/workflow/doctor"
	"strings"
)

// SbxState tri-state constants. Kept here (not workflow/doctor) because they
// are produced and consumed entirely within this package's probe.
const (
	SbxUnknown doctor.SbxState = iota // could not determine (sbx errored / no runner)
	SbxAbsent                         // sbx responded and the name is not present
	SbxRunning                        // present, status column reads running
	SbxStopped                        // present, any other status
)

// TaskSandboxStatus returns the status column for name from `sbx ls` via the
// seam (mirrors sandboxStatus but hermetic-testable), or "" when absent. This
// is the DISPLAY-only accessor; destructive decisions use the tri-state
// ProbeTaskSandbox instead, which never conflates errored with absent.
func TaskSandboxStatus(env hostenv.Env, name string) string {
	// BOUNDED (probeRun): a hung `sbx ls` yields "" (no display status), it
	// never wedges the caller.
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

// ProbeTaskSandbox classifies name from `sbx ls` via the seam into one of
// {running, stopped, absent, unknown}. A non-zero/errored sbx invocation (or a
// missing runner) is UNKNOWN, never absent, so a failed probe can never be read
// as "the sandbox was never created". BOUNDED (probeRun): a hung sbx times out
// to UNKNOWN — run/setup/task preflights degrade honestly instead of wedging.
func ProbeTaskSandbox(env hostenv.Env, name string) doctor.SbxState {
	out, timedOut, err := env.RunTimed("sbx", "ls")
	if timedOut || err != nil {
		return SbxUnknown
	}
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

// ResolveThroughMissing canonicalizes a path that may not exist yet, so it is
// comparable byte-for-byte with a filepath.EvalSymlinks'd sibling. EvalSymlinks
// fails outright on a missing path, so walk up to the deepest ancestor that DOES
// exist, resolve that, and re-append the segments below it. A path with no
// resolvable ancestor at all falls back to its cleaned absolute form. Kept as a
// general path utility (unrelated to tasks; used by setup's interrupt-resume
// path comparison) even though Story06 removed its original caller (task
// harvest's symlink-safety check).
func ResolveThroughMissing(abs string) string {
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
