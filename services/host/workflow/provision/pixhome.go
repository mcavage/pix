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
	// EnsureMemoryRemote registers name at url if unregistered. If name is
	// already registered, it reports whether the existing registration's URL
	// matches url — a mismatch is reported, never silently overwritten.
	EnsureMemoryRemote(name, url string) (matched bool, err error)
}

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

	// MemoryAuthToken is the bearer credential (container.
	// EnsureMemoryAuthToken's return value) embedded into the registered
	// MCP URL (security re-review HIGH finding). "" registers the bare
	// URL — only a caller that has not generated a token yet (or a test
	// exercising the pre-token shape) should ever pass that.
	MemoryAuthToken string
}

// Result reports what each step of Setup actually did.
type Result struct {
	Init             pixhome.InitResult
	ReleaseInstalled bool
	Container        container.Result
	MCPRegistered    bool
	// MCPMatched is meaningful only when MCPRegistered is true: false means
	// an existing registration under the reserved name points somewhere
	// else, and Setup left it untouched.
	MCPMatched bool
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

	// The whole allocate-the-port-then-reconcile-the-container sequence runs
	// under ONE lock (QA F4: "hold the setup lock through container
	// create/reconcile"). Without it, two concurrent `pix setup` runs against
	// the SAME PIX_HOME could each finish allocating (container.
	// AllocateMemoryPortLocked's own critical section is short) and only THEN
	// race each other's `docker create` of the identically-named container.
	// d.ContainerSpec's own HostPort is overridden here with whatever this
	// PIX_HOME actually has allocated — never a value the caller merely
	// guessed at (homeadapters.go's homeContainerSpec reads, never allocates).
	resolvedSpec := d.ContainerSpec
	err = sys.Lock(container.SetupLockPath(d.Home), func() error {
		port, perr := container.AllocateMemoryPortLocked(d.Home)
		if perr != nil {
			return fmt.Errorf("allocate pix-memory port: %w", perr)
		}
		resolvedSpec.HostPort = port
		cres, cerr := container.Reconcile(d.ContainerRunner, resolvedSpec, d.Prober, container.ReconcileOptions{
			ConfirmReplace: d.ConfirmReplace,
		})
		if cerr != nil {
			return fmt.Errorf("reconcile pix-memory container: %w", cerr)
		}
		res.Container = cres
		return nil
	})
	if err != nil {
		return res, err
	}

	// The Gateway registration URL carries the SAME resolved port the
	// container was just reconciled against — never d.ContainerSpec's
	// possibly-stale HostPort (QA F4: registration must never disagree with
	// what the container actually publishes).
	if d.MCP != nil {
		matched, err := d.MCP.EnsureMemoryRemote(envinfo.MCPMemoryName, container.MemoryMCPURL(resolvedSpec, d.MemoryAuthToken))
		if err != nil {
			return res, fmt.Errorf("register %s with the sbx Gateway: %w", envinfo.MCPMemoryName, err)
		}
		res.MCPRegistered = true
		res.MCPMatched = matched
	}

	return res, nil
}

// Ready reports whether Setup left the host in a state `pix doctor` would
// call fully ready: the container reconciled and probed successfully, and
// (when MCP registration was requested) the reserved name matches.
func (r Result) Ready() bool {
	if !r.Container.Ready() {
		return false
	}
	if r.MCPRegistered && !r.MCPMatched {
		return false
	}
	return true
}
