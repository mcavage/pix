// homeadapters.go — the concrete, production (non-fake) adapters the v2
// pixhome-based verbs (doctor, reset, setup) need to reach the real world:
// an os/exec-backed health.ExecChecker, an HTTP-backed container.Prober, and
// an `sbx mcp`-backed registrar/lister for the reserved pix-memory remote.
//
// `sbx mcp ls` has no machine-readable endpoint output. Setup verifies an
// existing endpoint when inspect/get supports it; otherwise it reconciles this
// stack's exact scoped name with remove-then-add instead of treating presence
// as readiness.
package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/mcp"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/stack"
	"pix/host/workflow/provision"
	"pix/host/workflow/reset"
)

// homeContainerSpec builds the container.Spec every v2 home caller
// reconciles/probes against: the release-pinned pix-memory image (when a
// release manifest is recorded; "" otherwise, which container.Inspect and
// the doctor probes both already treat as "nothing to compare against" and
// report absent/no-manifest rather than crashing on), this PIX_HOME's OWN
// allocated port (container.ReadMemoryPort — QA F4: a single fixed 18080
// forced two independent PIX_HOME instances into an inevitable collision;
// this is READ-ONLY here — it never allocates, only `pix setup`'s
// container.EnsureMemoryPort does that — so it degrades to
// container.DefaultMemoryPort for display purposes on a host that has never
// run `pix setup` yet, the same pre-setup posture the auth token below
// already has), this host's data directory, and — read-only, never
// generated here — whatever pix-memory bearer token file `pix setup` has
// already written, bind-mounted read-only at container.AuthTokenMountPath
// (security re-review round 1 blocker #1: never a literal `-e`/`--env-file`
// argument, both of which would land the token in the container's own
// Config.Env, which `docker inspect` exposes to anything on this host with
// inspect access). A caller that has not yet run `pix setup` sees an
// AuthTokenFile path that does not exist yet; that is fine for every
// current caller (doctor/run only ever read this spec, they never call
// container.Create with it — only setup_cmd.go does, and it always calls
// provision.EnsureMemoryAuthToken first).
func homeContainerSpec(home pixhome.Paths) container.Spec {
	image := ""
	if m, err := release.LoadInstalled(home.Home); err == nil && m != nil {
		image = provision.MemoryImageRef(*m)
	}
	port := container.DefaultMemoryPort
	if p, err := container.ReadMemoryPort(home); err == nil {
		port = p
	}
	// StackID/ContainerName are THIS PIX_HOME's own scoped identity (Wave B
	// coexistence: "one PIX_HOME = one stack") — never the bare legacy
	// container.Name. A home whose stack id cannot be derived (essentially
	// never: only a filepath.Abs failure) degrades to an empty ContainerName
	// rather than falling back to the unscoped name; every real caller of
	// this spec (doctor, run, setup) already treats an empty/absent name as
	// "nothing to compare against" rather than crashing.
	id, _ := stack.ID(home.Home)
	name, _ := stack.MemoryContainerName(id)
	return container.Spec{
		ContainerName: name,
		Image:         image,
		HostPort:      port,
		DataDir:       home.StateMemory,
		AuthTokenFile: container.MemoryAuthTokenPath(home),
		StackID:       id,
		Home:          home.Home,
	}
}

// execChecker is the production health.ExecChecker: run the named binary
// with args and report its combined output, exactly like every other small
// Runner interface in this module (pixhome.Runner, container.Runner).
type execChecker struct{}

func (execChecker) Check(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// startupProber absorbs the normal gap between `docker start` returning and
// pix-memory accepting health requests. It is used only by setup/upgrade;
// doctor keeps the same full probe one-shot so diagnostics stay fast.
type startupProber struct {
	Inner    container.Prober
	Timeout  time.Duration
	Interval time.Duration
}

func (p startupProber) Probe(baseURL string) error {
	if p.Inner == nil {
		return fmt.Errorf("startup readiness probe is not configured")
	}
	timeout, interval := p.Timeout, p.Interval
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	deadline := time.Now().Add(timeout)
	var last error
	for {
		last = p.Inner.Probe(baseURL)
		if last == nil {
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("pix-memory did not become ready within %s: %w", timeout, last)
		}
		if remaining < interval {
			time.Sleep(remaining)
		} else {
			time.Sleep(interval)
		}
	}
}

// sbxMemoryRegistrar is the production workflow/provision.MCPRegistrar for
// the reserved pix-memory remote: it shells to the real `sbx mcp` surface
// directly (the same sbx commands a human would run by hand), never a
// second client.
// See this file's own doc comment for the URL-visibility limitation that
// keeps this a register-once-if-absent adapter rather than a drift check.
type sbxMemoryRegistrar struct{}

// EnsureMemoryRemote registers name at url with the sbx Gateway. A matching
// readable endpoint is retained. A mismatched or unreadable endpoint under
// this stack-derived name is removed and re-added, which is the only way sbx
// 0.41 can earn current endpoint readiness without machine-readable listing.
func (sbxMemoryRegistrar) EnsureMemoryRemote(name, url string) (provision.MCPRegistrationState, error) {
	lsOut, _, lsErr := runSbxCapturedOut("mcp", "ls")
	if lsErr == nil {
		for _, n := range mcp.RegisteredNamesFrom(lsOut) {
			if n == name {
				// This host's own scoped name is already registered somewhere.
				// When inspect/get output actually lets us check the endpoint,
				// verify it rather than trusting the bare name match: a same
				// scoped name pointed at a DIFFERENT endpoint is a real drift,
				// never a silent "present" (round-4 review's own standard,
				// applied here too — never a success word nothing probed).
				matches, verified := mcp.VerifyExistingEndpoint(name, url)
				if verified && matches {
					return provision.MCPRegistrationPresentVerified, nil
				}
				return replaceMemoryRemote(name, url)
			}
		}
	}
	return addMemoryRemote(name, url)
}

func replaceMemoryRemote(name, url string) (provision.MCPRegistrationState, error) {
	_, stderr, err := runSbxCapturedOut("mcp", "rm", name)
	if err != nil {
		return provision.MCPRegistrationNone, fmt.Errorf("remove stale %s registration: %w: %s", name, err, strings.TrimSpace(stderr))
	}
	return addMemoryRemote(name, url)
}

func addMemoryRemote(name, url string) (provision.MCPRegistrationState, error) {
	// This reserved endpoint is intentionally loopback and token-authenticated,
	// so opt out of sbx's SSRF guard and hosted OAuth flow for this add only.
	_, stderr, err := runSbxCapturedOut("mcp", "add", name, "--url", url, "--skip-ssrf-check", "--skip_auth")
	if err != nil {
		return provision.MCPRegistrationNone, fmt.Errorf("%w: %s", err, container.RedactMemoryURLToken(strings.TrimSpace(stderr)))
	}
	return provision.MCPRegistrationAdded, nil
}

// sbxMemoryDeregistrar is the production workflow/reset.MCPDeregistrar for
// THIS stack's own scoped memory/session MCP registrations: `sbx mcp ls`
// proves presence before `sbx mcp rm` ever runs, and the removal is
// verified by name, never a bulk/global operation — `pix reset` never
// removes an MCP registration it did not first prove was registered under
// exactly this call's own name.
type sbxMemoryDeregistrar struct{}

func (sbxMemoryDeregistrar) RemoveIfRegistered(name string) (reset.MCPRemovalState, error) {
	lsOut, _, lsErr := runSbxCapturedOut("mcp", "ls")
	if lsErr != nil {
		return reset.MCPRemovalRetained, fmt.Errorf("list sbx MCP registrations: %w", lsErr)
	}
	present := false
	for _, n := range mcp.RegisteredNamesFrom(lsOut) {
		if n == name {
			present = true
			break
		}
	}
	if !present {
		return reset.MCPRemovalAbsent, nil
	}
	_, stderr, err := runSbxCapturedOut("mcp", "rm", name)
	if err != nil {
		return reset.MCPRemovalRetained, fmt.Errorf("remove %s: %w: %s", name, err, strings.TrimSpace(stderr))
	}
	return reset.MCPRemovalRemoved, nil
}

// runSbxCapturedOut is a tiny local wrapper so this file needs no export
// from mcp for the `sbx <args...>` exec shape; it mirrors mcp.go's own
// runSbxCaptured, which is unexported.
func runSbxCapturedOut(args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command("sbx", args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

// dispatchRun re-enters the ROOT for a composed `run` invocation (task's
// launch of a freshly created checkout), so a caller cannot acquire its own
// copy of run's grammar: it hands `run` an argv as a user would type it.
func dispatchRun(d *cli.Deps, argv []string) error {
	if code := dispatch(append([]string{"run"}, argv...), d); code != 0 {
		return cli.SilentError{Code: code}
	}
	return nil
}
