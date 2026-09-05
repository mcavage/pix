package reset

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"pix/host/container"
)

// pixhome.go is this unit's `pix reset` v2 path (docs/design/
// pix-v2-surface.md §3.8, pix-v2-architecture.md §12): remove Pix-owned
// sandboxes through the ordinary proof-gated planner (the SAME injected
// Sweep this package's v1 Run already uses — see Sweep's own doc comment),
// stop and remove the named memory container and PROVE it absent, and only
// then rename PIX_HOME aside with a timestamped backup suffix. There is no
// recursive delete anywhere in this file, and PIX_HOME itself is never
// renamed through a symlink — the same posture the v1 Run takes toward its
// own three directories, applied to the single v2 root.
//
// Order matters and is not incidental: a container failure or a proof
// failure must leave PIX_HOME exactly as it was, so ResetHome never renames
// anything until both prior steps are DONE, not merely attempted.

// Sweep is the injected sandbox teardown the command layer wires to `pix rm
// --all` (proof-gated, never a second force-removal seam) — see ResetHome's
// own doc comment.
type Sweep func(out, errOut io.Writer) error

// Description is the prose above reset's GENERATED usage: what the verb
// guarantees and in what order. The flag list is not here — the command
// struct's tags are the flag list.
const Description = `Clean slate: remove Pix-owned sandboxes, stop and remove the pix-memory
container, then rename PIX_HOME (default ~/.pix) aside with a timestamped
.bak-<unixts> suffix. REVERSIBLE: nothing is deleted, and the order is fixed —
sandboxes, then the memory container proven absent, then PIX_HOME — so a
failure at any step leaves everything before it untouched. Never follows a
symlinked PIX_HOME and never recurses into it.
`

// MCPRemovalState is what an MCPDeregistrar's RemoveIfRegistered actually
// proved about one name — or the honest default when nothing was even
// attempted. The zero value is the SAFE default (Go idiom: a caller that
// forgets to check still reads "retained", never a fabricated "removed").
type MCPRemovalState int

const (
	// MCPRemovalRetained: not attempted (no MCPDeregistrar wired, or the
	// name to remove was never resolved), or attempted but the outcome could
	// not be proven (a listing/removal call failed) — either way, whatever
	// registration exists under this name, if any, is left exactly as it
	// was found.
	MCPRemovalRetained MCPRemovalState = iota
	// MCPRemovalAbsent: a fresh listing positively showed this name was not
	// registered — nothing to remove.
	MCPRemovalAbsent
	// MCPRemovalRemoved: this name was registered and this run removed it.
	MCPRemovalRemoved
)

// MCPDeregistrar is reset's optional, best-effort seam for removing THIS
// stack's own scoped memory/session MCP registrations — never any other
// name, and never a bulk/global removal. A nil MCPDeregistrar (the honest
// default until a production adapter implements it) leaves every
// registration untouched; ResetHome reports that as MCPRemovalRetained,
// never as MCPRemovalRemoved — a state nothing proved is not reported as a
// success.
type MCPDeregistrar interface {
	// RemoveIfRegistered removes name from the sbx Gateway's registration
	// list ONLY when a fresh listing positively shows it present, and
	// reports which of the three honest outcomes (retained/absent/removed)
	// happened. An error means the attempt itself failed operationally
	// (e.g. the listing could not be fetched); ResetHome treats that the
	// same as MCPRemovalRetained — best-effort, never fatal to the reset.
	RemoveIfRegistered(name string) (MCPRemovalState, error)
}

// HomeDeps is everything ResetHome needs from the outside world, injected so
// a test drives the whole verb against a temp PIX_HOME with a fake sandbox
// sweep and a fake Docker runner — no real sbx, no real Docker, no real
// $HOME.
type HomeDeps struct {
	// Home is the resolved PIX_HOME root to rename aside.
	Home string
	// ContainerRunner talks to Docker for the named memory container.
	ContainerRunner container.Runner
	// ContainerName is THIS PIX_HOME's own scoped pix-memory container name
	// (stack.MemoryContainerName). It is REQUIRED — ResetHome refuses rather
	// than defaulting to the bare legacy container.Name, which could tear
	// down a completely different PIX_HOME's container on a host running
	// more than one stack.
	ContainerName string
	// MCP is the optional best-effort MCP-registration remover; see
	// MCPDeregistrar's own doc comment. nil (the default) means "not
	// supported by the current adapters" and every removal is reported
	// retained.
	MCP MCPDeregistrar
	// MemoryMCPName/SessionMCPName are THIS PIX_HOME's own scoped MCP names
	// (stack.MCPMemoryName/MCPSessionName) MCP removal targets, if MCP is
	// wired. Left empty, that name's removal is simply not attempted
	// (reported MCPRemovalRetained) — unlike ContainerName, an empty MCP
	// name is not a refusal, since MCP removal is advisory/best-effort, not
	// the proof-gated resource reset must remove before it may rename
	// PIX_HOME.
	MemoryMCPName, SessionMCPName string
	// Sweep is the SAME injected sandbox teardown Opts/Runtime already use
	// (`pix rm --all` in the command layer's words) — proof-gated, never a
	// second force-removal seam.
	Sweep Sweep
	// Out/ErrOut are where Sweep writes its own output/refusals.
	Out, ErrOut io.Writer
	// Now is injected for deterministic backup-suffix tests; nil uses
	// time.Now.
	Now func() time.Time
}

// HomeResult reports what ResetHome actually did.
type HomeResult struct {
	SweptSandboxes   bool
	ContainerRemoved bool
	// MemoryMCPState/SessionMCPState are best-effort: see MCPDeregistrar's
	// own doc comment. Their zero value (MCPRemovalRetained) is correct
	// whenever HomeDeps.MCP was nil or the matching *MCPName was empty.
	MemoryMCPState  MCPRemovalState
	SessionMCPState MCPRemovalState
	// BackupPath is the timestamped path PIX_HOME was renamed to, or "" if
	// PIX_HOME did not exist (nothing to rename — also not an error: reset
	// on a host that never ran setup is still a clean slate).
	BackupPath string
}

// removeMCPRegistrationOrRetained is the honest, best-effort wrapper every
// MCP removal attempt goes through: no deregistrar, or no name to remove,
// or an operational failure, all collapse to the same safe
// MCPRemovalRetained — never a fabricated "removed".
func removeMCPRegistrationOrRetained(d MCPDeregistrar, name string) MCPRemovalState {
	if d == nil || strings.TrimSpace(name) == "" {
		return MCPRemovalRetained
	}
	state, err := d.RemoveIfRegistered(name)
	if err != nil {
		return MCPRemovalRetained
	}
	return state
}

// ResetHome runs the v2 reset sequence in the one order that keeps every
// failure non-destructive: sweep sandboxes, then stop+remove+prove-absent
// the memory container, then rename PIX_HOME. A failure at any step returns
// before the next one runs, so a container that will not die never gets a
// renamed-out-from-under-it PIX_HOME, and a sweep failure never touches
// Docker or the filesystem at all.
func ResetHome(d HomeDeps) (HomeResult, error) {
	var res HomeResult
	now := d.Now
	if now == nil {
		now = time.Now
	}
	name := strings.TrimSpace(d.ContainerName)
	if name == "" {
		return res, fmt.Errorf("pix reset: internal: no container name given; refusing to guess (never defaults to the bare legacy %q name, which could remove a DIFFERENT PIX_HOME's container)", container.Name)
	}

	if d.Sweep != nil {
		if err := d.Sweep(d.Out, d.ErrOut); err != nil {
			return res, fmt.Errorf("pix reset: sweep sandboxes: %w", err)
		}
		res.SweptSandboxes = true
	}

	if err := container.StopAndRemove(d.ContainerRunner, name); err != nil {
		return res, fmt.Errorf("pix reset: stop/remove %s: %w", name, err)
	}
	absent, err := container.Absent(d.ContainerRunner, name)
	if err != nil {
		return res, fmt.Errorf("pix reset: probe %s absence: %w", name, err)
	}
	if !absent {
		return res, fmt.Errorf("pix reset: %s still exists after stop/remove; refusing to rename PIX_HOME while it might still be writing to .state/memory", name)
	}
	res.ContainerRemoved = true

	// Best-effort, never fatal: an MCP registration that could not be
	// removed (or was never even attempted) must not block the PIX_HOME
	// rename below — only the CONTAINER proof gates that.
	res.MemoryMCPState = removeMCPRegistrationOrRetained(d.MCP, d.MemoryMCPName)
	res.SessionMCPState = removeMCPRegistrationOrRetained(d.MCP, d.SessionMCPName)

	fi, err := os.Lstat(d.Home)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing to rename: a reset before any setup ever ran is still
			// a valid, clean-slate outcome.
			return res, nil
		}
		return res, fmt.Errorf("pix reset: stat %s: %w", d.Home, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return res, fmt.Errorf("pix reset: PIX_HOME (%s) is a symlink; refusing to rename through it", d.Home)
	}

	backup := fmt.Sprintf("%s.bak-%d", d.Home, now().Unix())
	if err := os.Rename(d.Home, backup); err != nil {
		return res, fmt.Errorf("pix reset: rename %s to %s: %w", d.Home, backup, err)
	}
	res.BackupPath = backup
	return res, nil
}
