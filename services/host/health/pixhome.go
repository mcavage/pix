package health

import (
	"context"
	"fmt"
	"strings"

	"pix/host/container"
	"pix/host/release"
)

// pixhome.go is this unit's read-only Pix v2 probe set
// (docs/design/pix-v2-surface.md §3.7, pix-v2-architecture.md §12): release
// identity, host prerequisites (Docker, Git), the pix-memory container's
// configuration/health, and its sbx Gateway MCP registration. Every probe
// here follows the package's existing contract — unknown is never ready and
// never absent, a fix is emitted only for a verified gap — and none of them
// mutates anything; that is `pix setup`'s job, not doctor's.

// Setup2Fix is the one repair command every v2 gap in this file names:
// `pix setup` is idempotent (docs/design/pix-v2-architecture.md §12), so
// re-running it is always the correct next action for a missing manifest, a
// missing/mismatched container, or a missing registration.
const Setup2Fix = "pix setup"

// ReleaseInstalledProbe verifies a release manifest is recorded at
// <home>/state/release.json and is well-formed (release.LoadInstalled
// already validates every field). It says nothing about whether the
// binaries it names still match what is actually running — that is a
// separate, evidence-bearing probe a later unit may add once there is a
// running binary to compare against.
type ReleaseInstalledProbe struct {
	// Home is the resolved PIX_HOME root.
	Home string
}

func (ReleaseInstalledProbe) Name() string   { return "pix-release" }
func (ReleaseInstalledProbe) Required() bool { return true }

func (p ReleaseInstalledProbe) Check(ctx context.Context) Result {
	m, err := release.LoadInstalled(p.Home)
	if err != nil {
		return Result{Status: StatusAbsent, Detail: "release manifest is invalid", Fix: Setup2Fix, Evidence: err.Error()}
	}
	if m == nil {
		return Result{Status: StatusAbsent, Detail: "no release installed yet", Fix: Setup2Fix,
			Evidence: fmt.Sprintf("%s does not exist", release.InstallStatePath(p.Home))}
	}
	return Result{Status: StatusReady, Detail: "release " + m.Version, Evidence: "state/" + release.InstallStateFile}
}

// ExecChecker is the injectable "is this binary usable" seam Docker/Git
// prerequisite probes share: a production caller wires it to
// exec.LookPath/exec.Command, a test wires it to a scripted double. It is
// intentionally smaller than health's existing runBounded machinery (which
// is private to this package and probes launcher-specific binaries); this
// unit's probes only need "found, and it answered".
type ExecChecker interface {
	// Check runs name with args and returns combined output and any error —
	// the exact shape container.Runner and pixhome.Runner already use.
	Check(name string, args ...string) (string, error)
}

// DockerAvailableProbe verifies the `docker` binary is present and answers
// `docker version`. Required because the pix-memory container cannot be
// reconciled at all without it (surface §2: "Docker Desktop or a Docker
// Engine configuration supported by sbx").
type DockerAvailableProbe struct{ Exec ExecChecker }

func (DockerAvailableProbe) Name() string   { return "docker" }
func (DockerAvailableProbe) Required() bool { return true }

func (p DockerAvailableProbe) Check(ctx context.Context) Result {
	if p.Exec == nil {
		return Result{Status: StatusUnknown, Detail: "no Docker checker configured"}
	}
	out, err := p.Exec.Check("docker", "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return Result{Status: StatusAbsent, Detail: "not available", Fix: "install Docker Desktop or a supported Docker Engine",
			Evidence: strings.TrimSpace(firstLine(out, err))}
	}
	return Result{Status: StatusReady, Detail: "docker " + strings.TrimSpace(out)}
}

// GitAvailableProbe verifies `git` is present — pixhome.Init needs it for
// `git init -b main` (architecture §5).
type GitAvailableProbe struct{ Exec ExecChecker }

func (GitAvailableProbe) Name() string   { return "git" }
func (GitAvailableProbe) Required() bool { return true }

func (p GitAvailableProbe) Check(ctx context.Context) Result {
	if p.Exec == nil {
		return Result{Status: StatusUnknown, Detail: "no git checker configured"}
	}
	out, err := p.Exec.Check("git", "--version")
	if err != nil {
		return Result{Status: StatusAbsent, Detail: "not available", Fix: "install Git",
			Evidence: strings.TrimSpace(firstLine(out, err))}
	}
	return Result{Status: StatusReady, Detail: strings.TrimSpace(out)}
}

func firstLine(out string, err error) string {
	if strings.TrimSpace(out) != "" {
		return out
	}
	return err.Error()
}

// MemoryContainerProbe verifies the named pix-memory container matches Spec
// and is healthy: exists, carries the ManagedLabel, its FingerprintLabel
// matches Spec's, is running, and probes ready. Every branch names the exact
// remedy architecture §9.1 promises: `pix setup` for anything Docker-state
// related (absent, unmanaged/mismatched, stopped), `docker restart
// pix-memory` only for a container that IS running the right config but
// fails its behavioral probe — "Docker restart policy restarts a dead
// process, not an unhealthy one."
type MemoryContainerProbe struct {
	Runner container.Runner
	Spec   container.Spec
	Prober container.Prober
}

func (MemoryContainerProbe) Name() string   { return "pix-memory" }
func (MemoryContainerProbe) Required() bool { return true }

func (p MemoryContainerProbe) Check(ctx context.Context) Result {
	name := p.Spec.ContainerName
	if strings.TrimSpace(name) == "" {
		name = container.Name
	}
	info, err := container.Inspect(p.Runner, name)
	if err != nil {
		return Result{Status: StatusUnknown, Detail: "could not inspect container", Evidence: err.Error()}
	}
	if !info.Exists {
		return Result{Status: StatusAbsent, Detail: "not created", Fix: Setup2Fix, Evidence: name + " does not exist"}
	}
	if !info.Managed() || info.Fingerprint() != p.Spec.Fingerprint() {
		return Result{Status: StatusAbsent, Detail: "configuration drift", Fix: Setup2Fix,
			Evidence: fmt.Sprintf("running image %q does not match the release-pinned configuration", info.Image)}
	}
	if !info.Running {
		return Result{Status: StatusAbsent, Detail: "stopped", Fix: Setup2Fix, Evidence: name + " exists but is not running"}
	}
	if p.Prober != nil {
		host := p.Spec.HostPort
		if err := p.Prober.Probe(fmt.Sprintf("http://127.0.0.1:%d", host)); err != nil {
			return Result{Status: StatusAbsent, Detail: "unhealthy", Fix: "docker restart " + name, Evidence: err.Error()}
		}
	}
	return Result{Status: StatusReady, Detail: "running", Evidence: "image " + info.Image}
}

// MCPLister is the injectable seam for "what is registered with the sbx
// Gateway": a production caller wires it to `sbx mcp ls --json` (or
// equivalent), a test wires it to a scripted map. It reports server name ->
// registered URL.
type MCPLister interface {
	ListMCP() (map[string]string, error)
}

// MemoryMCPRegistrationProbe verifies the reserved `pix-memory` MCP server
// name (envinfo.MCPMemoryName) is registered with the sbx Gateway and points
// at the exact loopback endpoint this host's container publishes — the
// registration `pix setup` owns (architecture §10: "Pix verifies that
// host-global registration matches the reviewed URL").
type MemoryMCPRegistrationProbe struct {
	Lister      MCPLister
	ServerName  string // defaults to "pix-memory"
	ExpectedURL string // e.g. "http://127.0.0.1:<port>/mcp"
}

func (MemoryMCPRegistrationProbe) Name() string   { return "pix-memory-mcp" }
func (MemoryMCPRegistrationProbe) Required() bool { return true }

func (p MemoryMCPRegistrationProbe) Check(ctx context.Context) Result {
	name := p.ServerName
	if strings.TrimSpace(name) == "" {
		name = container.Name
	}
	if p.Lister == nil {
		return Result{Status: StatusUnknown, Detail: "no sbx MCP lister configured"}
	}
	registered, err := p.Lister.ListMCP()
	if err != nil {
		return Result{Status: StatusUnknown, Detail: "could not list sbx MCP registrations", Evidence: err.Error()}
	}
	url, ok := registered[name]
	if !ok {
		return Result{Status: StatusAbsent, Detail: "not registered", Fix: Setup2Fix, Evidence: name + " has no sbx Gateway registration"}
	}
	if url != p.ExpectedURL {
		return Result{Status: StatusAbsent, Detail: "registration mismatch", Fix: Setup2Fix,
			Evidence: fmt.Sprintf("registered at %q, expected %q — never overwritten automatically", url, p.ExpectedURL)}
	}
	return Result{Status: StatusReady, Detail: "registered", Evidence: url}
}
