package launch

import (
	"fmt"
	"strings"

	"pix/host/envinfo"
	"pix/host/sandbox"
)

// WorkspaceMode is how the sandbox's project workspace reaches the host.
//
// It matters for recreation because the two modes have different data
// stakes. A DIRECT host mount is the host's own working tree: removing and
// recreating the sandbox around it cannot lose a byte, because the bytes
// were never inside the sandbox. A CLONE-mode workspace lives inside the
// sandbox, so unpushed commits die with it — that is precisely the case
// docs/design/pix-v2-surface.md §10 warns automatic recreation must never
// destroy. Unknown fails closed and is treated exactly like clone.
type WorkspaceMode int

const (
	// WorkspaceUnknown means the launcher could not positively determine
	// the mode. It never authorizes recreation.
	WorkspaceUnknown WorkspaceMode = iota
	// WorkspaceDirect means the host working tree is bind-mounted in.
	WorkspaceDirect
	// WorkspaceClone means the sandbox holds its own clone.
	WorkspaceClone
)

func (m WorkspaceMode) String() string {
	switch m {
	case WorkspaceDirect:
		return "direct"
	case WorkspaceClone:
		return "clone"
	default:
		return "unknown"
	}
}

// HolderCount is a holder census that can be UNKNOWN. A bare int cannot
// express "the refs directory could not be read", and a failed census that
// degraded to 0 would authorize a teardown of a sandbox somebody is
// actively using — the exact failure mode the flock design exists to
// prevent.
type HolderCount struct {
	Known bool
	N     int
}

// KnownHolders is a positively counted census.
func KnownHolders(n int) HolderCount { return HolderCount{Known: true, N: n} }

// UnknownHolders is the fail-closed census.
func UnknownHolders() HolderCount { return HolderCount{} }

// Zero reports a POSITIVE zero-holder answer. An unknown census is never
// zero.
func (h HolderCount) Zero() bool { return h.Known && h.N == 0 }

// RecreateProof is everything beyond drift classification that an
// automatic remove-and-recreate requires. Every field is a POSITIVE fact
// the caller proved on THIS launch; a zero value authorizes nothing.
//
// The proof set is deliberately the same one proof-gated teardown already
// demands (workflow/launch.TeardownSandbox), plus the workspace-mode fact
// that separates "rebuilding a container around the host's files" from
// "deleting the only copy of somebody's commits".
type RecreateProof struct {
	// FreshListing is true when the sbx listing behind AttachGate.Entry
	// was taken on this launch, not read from a record. A stale listing
	// cannot prove liveness.
	FreshListing bool
	// Holders is the session-tree holder census for this sandbox.
	Holders HolderCount
	// Keep is true when a keep marker exists. A kept sandbox is never
	// recreated automatically, whatever drifted.
	Keep bool
	// Workspace is how the project workspace reaches the host.
	Workspace WorkspaceMode
}

// blockers returns every reason this proof does NOT authorize automatic
// recreation, in a stable order, already worded for a user.
func (p RecreateProof) blockers() []string {
	var out []string
	if !p.FreshListing {
		out = append(out, "its sbx listing was not re-read on this launch (no fresh liveness proof)")
	}
	if !p.Holders.Known {
		out = append(out, "its live-holder count could not be determined")
	} else if p.Holders.N > 0 {
		out = append(out, fmt.Sprintf("%d live session node(s) still hold it", p.Holders.N))
	}
	if p.Keep {
		out = append(out, "it carries a keep marker")
	}
	switch p.Workspace {
	case WorkspaceDirect:
	case WorkspaceClone:
		out = append(out, "its workspace is a sandbox-side clone, so recreating it could destroy unpushed commits")
	default:
		out = append(out, "its workspace mode could not be determined")
	}
	return out
}

// Authorizes reports whether this proof clears every non-drift gate.
func (p RecreateProof) Authorizes() bool { return len(p.blockers()) == 0 }

// RecreatePlan is the decided remove-then-create sequence for a
// recreation-safe drift. Both steps are already-composed argv, so the
// caller runs exactly what was decided and nothing re-derives a name or a
// file path between the decision and the mutation.
type RecreatePlan struct {
	// SandboxName is the exact pix-* name being recreated.
	SandboxName string
	// InstanceID is the instance the removal must still match. Removal is
	// refused if the live instance is anything else.
	InstanceID string
	// Reason is the one-line explanation printed before mutating.
	Reason string
	// Drifts is the classified, recreation-safe drift set.
	Drifts []envinfo.Drift
}

// PlanSafeRecreate decides whether drifts + proof authorize automatically
// removing and recreating name. It is pure: it mutates nothing and runs no
// command. A nil plan with a non-empty refusal reason means "refuse and
// tell the user exactly why"; a nil plan with an empty reason means the
// drift set was not recreation-safe at all.
func PlanSafeRecreate(name, instanceID string, drifts []envinfo.Drift, proof RecreateProof) (*RecreatePlan, []string) {
	if !envinfo.RecreationSafe(drifts) {
		return nil, nil
	}
	if b := proof.blockers(); len(b) > 0 {
		return nil, b
	}
	return &RecreatePlan{
		SandboxName: name,
		InstanceID:  instanceID,
		Reason:      recreateReason(drifts),
		Drifts:      drifts,
	}, nil
}

// recreateReason words the drift set as one line a user can act on.
func recreateReason(drifts []envinfo.Drift) string {
	keys := make([]string, 0, len(drifts))
	for _, d := range drifts {
		keys = append(keys, d.ComposedKey)
	}
	return fmt.Sprintf("pinned sandbox construction changed (%s); removing and recreating it",
		strings.Join(keys, ", "))
}

// RemoveArgvFor is the removal step of a recreate plan: the same
// name-scoped, instance-checked removal path `pix rm NAME` uses. It never
// composes a forced removal — automatic recreation gets no authority
// override, only the ordinary proof-gated seam.
func (p *RecreatePlan) RemoveArgvFor(effectivePath string) (EnvRemovalPlan, error) {
	return PlanEnvRemoveSeam(effectivePath, p.SandboxName, false)
}

// scopedName re-asserts the pix-* namespace at the last moment before a
// recreate mutates anything. The name was derived by sandbox.NameFor, so
// this can only fail on a caller bug — which is exactly when a namespace
// check is worth having.
func scopedName(name string) error {
	if !strings.HasPrefix(name, sandbox.Prefix) {
		return fmt.Errorf("pix: refusing to recreate %q: not a pix-owned sandbox name", name)
	}
	return nil
}

// Validate checks a plan immediately before it is executed.
func (p *RecreatePlan) Validate() error {
	if p == nil {
		return fmt.Errorf("pix: no recreate plan")
	}
	if err := scopedName(p.SandboxName); err != nil {
		return err
	}
	if strings.TrimSpace(p.InstanceID) == "" {
		return fmt.Errorf("pix: refusing to recreate %q: no recorded instance id to match", p.SandboxName)
	}
	return nil
}
