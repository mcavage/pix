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
	"pix/host/hostenv"
	"pix/host/sandbox"
	"strings"
)

// SbxState is a package-local alias for the canonical sandbox.State (see
// sandbox/list.go): the four-state a sandbox probe resolves to. It exists so
// every existing `launch.SbxState`/`launch.SbxXxx` caller and test keeps
// compiling unchanged now that the type itself lives in the L1 sandbox
// package instead of being duplicated here.
type SbxState = sandbox.State

// SbxUnknown/Absent/Running/Stopped are launch's stable names for
// sandbox.State's four values. Kept here (not just sandbox.StateXxx)
// because they are produced and consumed entirely within this package's
// probe, and renaming every existing call site to the sandbox spelling would
// be a pure churn edit with no behavior change.
const (
	SbxUnknown = sandbox.StateUnknown // could not determine (sbx errored / no runner)
	SbxAbsent  = sandbox.StateAbsent  // sbx responded and the name is not present
	SbxRunning = sandbox.StateRunning // present, status column reads running
	SbxStopped = sandbox.StateStopped // present, any other status
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
func ProbeTaskSandbox(env hostenv.Env, name string) SbxState {
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
