package task

// SandboxDisposition is the typed value a caller (Story04/L4, the launch
// package) passes IN to RemoveGuard/List. This package never probes for a
// sandbox's liveness itself — it only reasons about the value it is given.
type SandboxDisposition int

const (
	// SandboxUnknown means the caller could not determine the sandbox's
	// state (e.g. `sbx ls` failed). Fail-safe: treated the same as running.
	SandboxUnknown SandboxDisposition = iota
	// SandboxAbsent means no sandbox by that name exists.
	SandboxAbsent
	// SandboxStopped means the sandbox exists but is not running.
	SandboxStopped
	// SandboxRunning means the sandbox is live.
	SandboxRunning
)

func (d SandboxDisposition) String() string {
	switch d {
	case SandboxAbsent:
		return "absent"
	case SandboxStopped:
		return "stopped"
	case SandboxRunning:
		return "running"
	default:
		return "unknown"
	}
}

// GitState is the checkout's git-hygiene facts, as gathered by
// GatherGitState. It carries no sandbox knowledge.
type GitState struct {
	Dirty         bool // tracked files have uncommitted modifications
	Untracked     bool // untracked files are present
	HasUpstream   bool // the branch has a configured upstream
	Ahead         int  // commits ahead of upstream (display only; 0 when no upstream)
	Unrecoverable int  // commits reachable only from this checkout, not from mainroot or its own remotes (0 for Worktree: nothing is ever checkout-only there)
	Unknown       bool // a probe failed; fail-safe (treated as if there might be unrecoverable work)
}

// RemoveGuard is the pure removal decision. The sandbox disposition is NEVER
// overridden by force — a live or indeterminate sandbox always refuses,
// because tearing one down is Story04/L4's call, not this package's, and
// force here only ever means "override GIT hygiene". Once the sandbox is
// not in the way, force additionally overrides dirty/untracked/unrecoverable
// git-hygiene reasons.
func RemoveGuard(git GitState, sandbox SandboxDisposition, force bool) (reasons []string, ok bool) {
	switch sandbox {
	case SandboxRunning:
		reasons = append(reasons, "the sandbox is still running; stop it first")
	case SandboxUnknown:
		reasons = append(reasons, "cannot determine the sandbox's state; resolve, then retry")
	}
	if len(reasons) > 0 {
		return reasons, false
	}
	if force {
		return nil, true
	}
	if git.Unknown {
		reasons = append(reasons, "could not determine this checkout's git state (a git probe failed)")
	}
	if git.Dirty {
		reasons = append(reasons, "the checkout has uncommitted changes")
	}
	if git.Untracked {
		reasons = append(reasons, "the checkout has untracked files")
	}
	if git.Unrecoverable > 0 {
		reasons = append(reasons, "commit(s) exist only in this checkout and are not reachable from the main repo")
	}
	return reasons, len(reasons) == 0
}
