package provision

import (
	"fmt"

	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/stack"
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
// for this stack's reserved pix-memory remote MCP server. A production caller
// verifies a matching endpoint or reconciles that exact stack-derived name with
// `sbx mcp rm` plus `sbx mcp add`; a test wires it to a fake registry.
type MCPRegistrar interface {
	// EnsureMemoryRemote leaves a readable matching endpoint alone and otherwise
	// reconciles the exact stack-derived name or returns an honest failure. The
	// state type retains PresentUnverified for injected implementations that
	// cannot safely reconcile; Ready never treats that state as success.
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
	// MCPRegistrationPresentVerified: a registration already existed under
	// the reserved name, was left untouched, AND this host read its endpoint
	// back and it EQUALS the URL setup would have registered. This is the
	// only "already fine" answer, and a probe earned it.
	MCPRegistrationPresentVerified
	// MCPRegistrationPresentUnverified: a registration already existed under
	// the reserved name and was left untouched, but its endpoint could NOT be
	// read back (`sbx mcp ls` does not print endpoints, and inspect/get both
	// failed). Neither a match nor a mismatch: this host genuinely cannot
	// tell what the sandbox would reach through that name. It is NOT ready
	// (Ready() is false), because "there is something under this name" is not
	// evidence that memory works: laundering it into one is exactly the
	// success word safety invariant 12 forbids. Nothing is overwritten or
	// removed either way; the caller states the manual check.
	MCPRegistrationPresentUnverified
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
	// A production caller ALWAYS passes a nonzero manifest, discovered
	// from the installed release bundle before any Docker or Gateway
	// mutation (architecture §12 steps 3-4).
	Manifest release.Manifest

	// Bundle is the discovered release bundle whose runtime archive this
	// call installs under PIX_HOME (architecture §12 step 3). nil skips
	// the runtime install; when non-nil its manifest must equal Manifest.
	Bundle *release.Bundle

	// Prereqs verifies Docker/sbx/Git BEFORE anything is mutated. nil
	// skips the check (a test driving only later steps).
	Prereqs PrereqChecker

	// EnsureImages verifies/pulls both digest-pinned images. nil skips
	// it; production wires it to the package's EnsureImages against the
	// same Docker runner the container steps use.
	EnsureImages func(m release.Manifest) error

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
	// Runtime is what the runtime-archive install did (architecture §12
	// step 3). Its zero value means no bundle was supplied.
	Runtime release.RuntimeInstall
	// DefaultEnvCreated is true only when this run created the `default`
	// environment because the host had none (step 5).
	DefaultEnvCreated bool
	// KitRevision is the strict kit identity the installed manifest pins,
	// carried here so a caller need not re-read the manifest to report it.
	KitRevision   string
	Container     container.Result
	MCPRegistered bool
	// MCPState is what the registrar observed; meaningful only when
	// MCPRegistered is true.
	MCPState MCPRegistrationState
	// MCPName is the scoped registration name this run acted on, carried so
	// a caller can print an EXACT remedy (the name to inspect or remove)
	// without re-deriving it from a stack id of its own.
	MCPName string
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

	// Step 1: prerequisites, before ANY mutation. A host missing Docker
	// must not first acquire a half-initialized home, a bearer token, and
	// a Gateway registration nothing can serve.
	if err := CheckPrereqs(d.Prereqs); err != nil {
		return res, err
	}

	if d.Bundle != nil && d.Bundle.Manifest != d.Manifest {
		return res, fmt.Errorf("internal: setup was given a release bundle (%s) and a different manifest (%s)", d.Bundle.Manifest.Version, d.Manifest.Version)
	}

	initRes, err := pixhome.Init(d.Home, d.GitRunner)
	if err != nil {
		return res, fmt.Errorf("initialize pix home: %w", err)
	}
	res.Init = initRes

	if d.Manifest != (release.Manifest{}) {
		if err := d.Manifest.Validate(); err != nil {
			return res, fmt.Errorf("release manifest: %w", err)
		}
		// Step 3: install the runtime archive FIRST, then record the
		// manifest. The recorded manifest is what doctor and every later
		// launch treat as "what is installed", so it must never claim a
		// release whose runtime content failed to land.
		if d.Bundle != nil {
			rt, err := release.InstallRuntime(d.Home.Home, *d.Bundle)
			if err != nil {
				return res, fmt.Errorf("install the runtime archive: %w", err)
			}
			res.Runtime = rt
		}
		if err := release.SaveInstalled(d.Home.Home, d.Manifest); err != nil {
			return res, fmt.Errorf("record release manifest: %w", err)
		}
		res.ReleaseInstalled = true
		res.KitRevision = runtimeKitRevision(d.Manifest)

		// Step 4: both digest-pinned images must be obtainable before the
		// container step tries to run one of them.
		if d.EnsureImages != nil {
			if err := d.EnsureImages(d.Manifest); err != nil {
				return res, err
			}
		}

		// Step 5: a host with no environment at all gets exactly one
		// minimal, runnable `default`. An existing environment is never
		// touched, merged, or rewritten.
		created, err := EnsureDefaultEnvironment(d.Home, d.Manifest)
		if err != nil {
			return res, fmt.Errorf("create the default environment: %w", err)
		}
		res.DefaultEnvCreated = created
	}

	// stackID is THIS PIX_HOME's own coexistence identity (Wave B: "one
	// PIX_HOME = one stack") — derived here, once, and used for BOTH the
	// container's ownership labels and the reserved MCP name Setup
	// registers, so the two can never independently drift onto different
	// scoping. There is no unscoped fallback: a home whose id cannot be
	// derived refuses rather than silently reconciling/registering under a
	// bare legacy name.
	stackID, err := stack.ID(d.Home.Home)
	if err != nil {
		return res, fmt.Errorf("derive this PIX_HOME's stack id: %w", err)
	}

	// The whole generate-token, allocate-port, reconcile-container sequence
	// runs under ONE lock (QA F4: "hold the setup lock through container
	// create/reconcile"; round-4 security: the auth token is generated in
	// here too, so two concurrent setups against the SAME PIX_HOME cannot
	// mint two tokens and then race which one the container was created
	// against). d.ContainerSpec's own HostPort is overridden with whatever
	// this PIX_HOME actually has allocated — never a value the caller
	// merely guessed at (homeadapters.go's homeContainerSpec reads, never
	// allocates). StackID/Home are ALWAYS stamped here too, authoritatively,
	// regardless of what the caller's ContainerSpec already carried: Setup is
	// the one place that actually creates/reconciles this container, so it
	// is the one place that must guarantee the ownership labels are correct.
	resolvedSpec := d.ContainerSpec
	resolvedSpec.StackID = stackID
	resolvedSpec.Home = d.Home.Home
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
		memoryName, nerr := stack.MCPMemoryName(stackID)
		if nerr != nil {
			return res, fmt.Errorf("derive this PIX_HOME's scoped pix-memory MCP name: %w", nerr)
		}
		res.MCPName = memoryName
		state, err := d.MCP.EnsureMemoryRemote(memoryName, container.MemoryMCPURL(resolvedSpec, token))
		if err != nil {
			return res, fmt.Errorf("register %s with the sbx Gateway: %w", memoryName, err)
		}
		res.MCPRegistered = true
		res.MCPState = state
	}

	return res, nil
}

// Ready reports whether Setup left the host in a state `pix doctor` would
// call fully ready: the container reconciled and probed successfully, and
// (when registration was requested) the registrar reached an outcome a probe
// actually earned: this run registered the name itself, or read the existing
// registration's endpoint back and it matched.
//
// An UNVERIFIED presence is not ready. Nothing observed a defect, but nothing
// observed the endpoint either, and "a server exists under this name" is not
// the claim setup makes; the sandbox could be pointed at another home's
// memory, or at nothing. The caller reports that state in those words and
// names the manual check (see renderSetupResult).
func (r Result) Ready() bool {
	if !r.Container.Ready() {
		return false
	}
	if r.MCPRegistered && (r.MCPState == MCPRegistrationNone || r.MCPState == MCPRegistrationPresentUnverified) {
		return false
	}
	return true
}
