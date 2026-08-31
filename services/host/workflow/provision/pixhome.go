package provision

import (
	"fmt"

	"pix/host/container"
	"pix/host/envinfo"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/sys"
)

// pixhome.go is this unit's `pix setup` v2 path (docs/design/
// pix-v2-surface.md §3.6, pix-v2-architecture.md §12): idempotently
// initialize PIX_HOME, record the installed release manifest, reconcile the
// one named pix-memory container, and register/verify its reserved sbx
// remote MCP name. It composes L0/L1/L2 (pixhome, release, container,
// envinfo) the way provision already does for the v1 setup flow in this
// same package, and mutates nothing `Setup` was not explicitly asked to.

// MCPRegistrar is the injectable "register this with the sbx Gateway" seam
// for the reserved pix-memory remote MCP server (architecture §10: "Pix
// verifies that host-global registration matches the reviewed URL or local
// command... never overwritten automatically"). A production caller wires
// it to `sbx mcp add`/`sbx mcp ls`; a test wires it to a fake registry.
type MCPRegistrar interface {
	// EnsureMemoryRemote registers name at url if unregistered and reports
	// which of the two honest outcomes happened. An already-registered name
	// is NEVER overwritten and NEVER reported as verified: a host that
	// cannot read back the existing entry's URL says so (MCPPresent), it
	// does not launder "there is something under this name" into "it points
	// where we want".
	EnsureMemoryRemote(name, url string) (state MCPRegistrationState, err error)
}

// MCPRegistrationState is what a registrar actually observed, kept distinct
// from what it would like to be true (round-4 review: `pix setup` printed
// "ok" for an existing registration whose URL this host cannot read, which
// is a success word nothing probed — safety invariant 12).
type MCPRegistrationState int

const (
	// MCPRegistrationNone: no registrar was wired, so nothing was attempted.
	MCPRegistrationNone MCPRegistrationState = iota
	// MCPRegistrationAdded: this run registered the name at the URL it
	// composed, and the registrar's own add command reported success.
	MCPRegistrationAdded
	// MCPRegistrationPresent: a registration already existed under the
	// reserved name and was left untouched. Its URL is NOT verifiable from
	// this host (`sbx mcp ls` does not print endpoints), so this is neither
	// a match nor a mismatch — it is an unverifiable presence.
	MCPRegistrationPresent
)

// Deps is every external effect Setup needs, injected so a test drives the
// whole flow against a temp PIX_HOME with fake docker/git/MCP doubles.
type Deps struct {
	Home pixhome.Paths

	GitRunner       pixhome.Runner   // nil uses pixhome.DefaultRunner
	ContainerRunner container.Runner // nil uses container.DefaultRunner
	Prober          container.Prober

	// Manifest is the release this setup call installs. Empty
	// (zero-value) means "do not touch the release record" — a caller
	// still iterating on the container/MCP steps need not fabricate one.
	Manifest release.Manifest

	// ContainerSpec is the pix-memory container Reconcile targets. Its
	// Image is normally derived from Manifest.PixMemoryDigest by the
	// caller, not by this function — Setup does not invent an image
	// reference from a manifest field name, since that coupling belongs
	// where the manifest schema and the spec are both in view.
	ContainerSpec container.Spec

	// ConfirmReplace is forwarded to container.Reconcile unchanged — see
	// its own doc comment for the "show then replace mismatch" contract.
	ConfirmReplace func(current container.Info, want container.Spec) bool

	// MCP registers/verifies the reserved pix-memory remote. nil skips
	// that step entirely (e.g. a caller testing only the home/container
	// halves).
	MCP MCPRegistrar

	// MaxPortRetries bounds the reallocate-on-bind-conflict loop below.
	// Zero means DefaultPortRetries; a caller never needs to set it except
	// a test proving the bound is real.
	MaxPortRetries int
}

// DefaultPortRetries is how many times Setup will draw a fresh loopback
// port after `docker create` loses a bind race with another PIX_HOME.
// Bounded on purpose: a host where three consecutive OS-assigned free ports
// are all stolen before the bind has a problem no retry loop can fix, and
// the user deserves the real error rather than an unbounded spin.
const DefaultPortRetries = 3

// Result reports what each step of Setup actually did.
type Result struct {
	Init             pixhome.InitResult
	ReleaseInstalled bool
	Container        container.Result
	MCPRegistered    bool
	// MCPState is what the registrar observed; meaningful only when
	// MCPRegistered is true.
	MCPState MCPRegistrationState
	// MemoryPort is the loopback port this PIX_HOME's pix-memory actually
	// got, after any bind-conflict reallocation. It is the port the
	// registered Gateway URL names.
	MemoryPort int
}

// Setup runs the v2 setup sequence: initialize PIX_HOME, record the release
// manifest, reconcile the memory container, then register/verify its
// reserved MCP remote. Every step is idempotent; rerunning Setup after a
// prior successful run changes nothing except what actually drifted.
func Setup(d Deps) (Result, error) {
	var res Result

	initRes, err := pixhome.Init(d.Home, d.GitRunner)
	if err != nil {
		return res, fmt.Errorf("initialize pix home: %w", err)
	}
	res.Init = initRes

	if d.Manifest != (release.Manifest{}) {
		if err := d.Manifest.Validate(); err != nil {
			return res, fmt.Errorf("release manifest: %w", err)
		}
		if err := release.SaveInstalled(d.Home.Home, d.Manifest); err != nil {
			return res, fmt.Errorf("record release manifest: %w", err)
		}
		res.ReleaseInstalled = true
	}

	// The whole generate-token, allocate-port, reconcile-container sequence
	// runs under ONE lock (QA F4: "hold the setup lock through container
	// create/reconcile"; round-4 security: the auth token is generated in
	// here too, so two concurrent setups against the SAME PIX_HOME cannot
	// mint two tokens and then race which one the container was created
	// against). d.ContainerSpec's own HostPort is overridden with whatever
	// this PIX_HOME actually has allocated — never a value the caller
	// merely guessed at (homeadapters.go's homeContainerSpec reads, never
	// allocates).
	resolvedSpec := d.ContainerSpec
	var token string
	retries := d.MaxPortRetries
	if retries <= 0 {
		retries = DefaultPortRetries
	}
	err = sys.Lock(container.SetupLockPath(d.Home), func() error {
		tok, terr := container.EnsureMemoryAuthToken(d.Home)
		if terr != nil {
			return fmt.Errorf("pix-memory auth token: %w", terr)
		}
		token = tok

		port, perr := container.AllocateMemoryPortLocked(d.Home)
		if perr != nil {
			return fmt.Errorf("allocate pix-memory port: %w", perr)
		}
		// Bind conflicts are the ONE reconcile failure a different port can
		// fix, and they are reachable even with a correct allocation: the
		// port was free when this home recorded it and another PIX_HOME (or
		// any other process) took it before docker bound it. Each retry
		// PERSISTS the new port under this same lock before reconciling, so
		// the container that finally starts and the Gateway URL registered
		// below always name the identical port — no window where config.toml
		// says one thing and Docker another.
		for attempt := 0; ; attempt++ {
			resolvedSpec.HostPort = port
			cres, cerr := container.Reconcile(d.ContainerRunner, resolvedSpec, d.Prober, container.ReconcileOptions{
				ConfirmReplace: d.ConfirmReplace,
			})
			if cerr == nil {
				res.Container = cres
				return nil
			}
			if !container.IsPortInUse(cerr) || attempt >= retries {
				return fmt.Errorf("reconcile pix-memory container: %w", cerr)
			}
			p, rerr := container.ReallocateMemoryPortLocked(d.Home)
			if rerr != nil {
				return fmt.Errorf("reallocate pix-memory port after %v: %w", cerr, rerr)
			}
			port = p
		}
	})
	if err != nil {
		return res, err
	}
	res.MemoryPort = resolvedSpec.HostPort

	// The Gateway registration URL carries the SAME resolved port the
	// container was just reconciled against — never d.ContainerSpec's
	// possibly-stale HostPort (QA F4: registration must never disagree with
	// what the container actually publishes), and the SAME token generated
	// under the lock above.
	if d.MCP != nil {
		state, err := d.MCP.EnsureMemoryRemote(envinfo.MCPMemoryName, container.MemoryMCPURL(resolvedSpec, token))
		if err != nil {
			return res, fmt.Errorf("register %s with the sbx Gateway: %w", envinfo.MCPMemoryName, err)
		}
		res.MCPRegistered = true
		res.MCPState = state
	}

	return res, nil
}

// Ready reports whether Setup left the host in a state `pix doctor` would
// call fully ready: the container reconciled and probed successfully, and
// (when registration was requested) the registrar reached a definite
// outcome. An unverifiable PRESENT registration is not a failure — nothing
// observed a defect — but the caller must not print it as verified either;
// see renderSetupResult's wording.
func (r Result) Ready() bool {
	if !r.Container.Ready() {
		return false
	}
	if r.MCPRegistered && r.MCPState == MCPRegistrationNone {
		return false
	}
	return true
}
