package health

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
// <home>/.state/release.json and is well-formed (release.LoadInstalled
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
	name := strings.TrimSpace(p.Spec.ContainerName)
	if name == "" {
		// No unscoped fallback: an empty ContainerName means this PIX_HOME's
		// stack id could not be derived (homeContainerSpec's own posture), not
		// "assume the bare legacy pix-memory" — that could probe a totally
		// unrelated stack's container.
		return Result{Status: StatusUnknown, Detail: "no container name configured"}
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
	name := strings.TrimSpace(p.ServerName)
	if name == "" {
		// No unscoped fallback — see MemoryContainerProbe's own comment.
		return Result{Status: StatusUnknown, Detail: "no MCP server name configured"}
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

// EnvironmentDefaultProbe verifies this host has a MACHINE DEFAULT
// environment selected once at least one environment directory exists.
// Doctor is read-only (its own package doc: "never repairs, registers,
// restarts, authenticates, or rewrites configuration"), so a host with
// environments but no default is NEVER guessed at here — the exact remedy
// is always `pix env default NAME`, naming the one command that owns this
// field (config.SetDefaultEnvironmentAt).
//
// A host with ZERO environments is not a gap this probe reports: `pix run`
// still works against the built-in-defaults document (D17's `none`), and
// `pix setup` is what creates a first environment (EnsureDefaultEnvironment
// already selects it the moment it creates one, so this state should not
// even arise for a host that ran setup) — there is nothing for a user to
// point this probe's fix at yet.
type EnvironmentDefaultProbe struct {
	// Home is the resolved PIX_HOME root.
	Home string
	// DefaultEnvironment is the loaded config's own field — passed in rather
	// than re-loaded, so this probe can never disagree with the same config
	// every other probe and the render already share.
	DefaultEnvironment string
}

func (EnvironmentDefaultProbe) Name() string   { return "environment" }
func (EnvironmentDefaultProbe) Required() bool { return false }

func (p EnvironmentDefaultProbe) Check(ctx context.Context) Result {
	if strings.TrimSpace(p.DefaultEnvironment) != "" {
		return Result{Status: StatusReady, Detail: "default environment selected", Evidence: p.DefaultEnvironment}
	}
	names, err := environmentNames(p.Home)
	if err != nil {
		return Result{Status: StatusUnknown, Detail: "could not list environments", Evidence: err.Error()}
	}
	if len(names) == 0 {
		return Result{Status: StatusOff, Detail: "no environment yet", Hint: "run `pix setup` to create a runnable default environment"}
	}
	return Result{Status: StatusAbsent, Detail: "no default environment selected", Fix: "pix env default NAME",
		Evidence: fmt.Sprintf("found %d environment(s) (%s) and no machine default", len(names), strings.Join(names, ", "))}
}

// environmentNames lists the plain-directory environment names under
// <home>/envs — the SAME "a directory, not a dotfile" rule
// provision.EnsureDefaultEnvironment already applies, kept as an independent
// tiny scan here rather than an import of workflow/env: this package stays
// dependency-light (pixhome.go's own doc comment) and a name-only listing
// needs no sidecar parse, no trust check, no BoM.
func environmentNames(home string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(home, "envs"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}
