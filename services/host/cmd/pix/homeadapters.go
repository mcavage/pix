// homeadapters.go — the concrete, production (non-fake) adapters the v2
// pixhome-based verbs (doctor, reset, setup) need to reach the real world:
// an os/exec-backed health.ExecChecker, an HTTP-backed container.Prober, and
// an `sbx mcp`-backed registrar/lister for the reserved pix-memory remote.
//
// `sbx mcp ls` has no machine-readable endpoint output. The registrar can
// prove a name absent, but an existing same-name registration is left alone;
// it cannot honestly claim the URL matches. Doctor therefore reports that
// registration as unverifiable rather than inventing a drift signal.
package main

import (
	"bytes"
	"fmt"
	"net/http"
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

// httpProber is the production container.Prober: a bounded GET against
// baseURL+"/healthz", the non-MCP liveness endpoint architecture §9.1
// reserves for exactly this check. It never attempts an MCP
// initialize/tools-list handshake itself — that needs a real MCP client,
// which is the Gateway's job, not doctor's or setup's.
type httpProber struct{ Client *http.Client }

func (p httpProber) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return &http.Client{Timeout: 3 * time.Second}
}

func (p httpProber) Probe(baseURL string) error {
	resp, err := p.client().Get(baseURL + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpStatusError{URL: baseURL + "/healthz", Code: resp.StatusCode}
	}
	return nil
}

type httpStatusError struct {
	URL  string
	Code int
}

func (e *httpStatusError) Error() string {
	return "GET " + e.URL + ": unexpected status " + http.StatusText(e.Code)
}

// sbxMemoryRegistrar is the production workflow/provision.MCPRegistrar for
// the reserved pix-memory remote: it shells to the real `sbx mcp` surface
// directly (the same sbx commands a human would run by hand), never a
// second client.
// See this file's own doc comment for the URL-visibility limitation that
// keeps this a register-once-if-absent adapter rather than a drift check.
type sbxMemoryRegistrar struct{}

// EnsureMemoryRemote registers name at url with the sbx Gateway if it is not
// already present in `sbx mcp ls`'s listing. An existing registration under
// name is left untouched — the one unconditional rule architecture §10
// states regardless of what this host can observe — and reported as
// MCPRegistrationPresent, never as a match: `sbx mcp ls` does not print
// endpoints, so this host genuinely cannot tell whether the existing entry
// points at the URL we would have registered (round-4 review: reporting it
// as "ok" was a success word no probe earned).
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
				if matches, verified := mcp.VerifyExistingEndpoint(name, url); verified && !matches {
					return provision.MCPRegistrationNone, fmt.Errorf(
						"%s is already registered with the sbx Gateway at a different endpoint than %s; refusing to overwrite it (remove it manually first if you want pix to re-register it: sbx mcp rm %s)",
						name, container.RedactMemoryURLToken(url), name)
				}
				return provision.MCPRegistrationPresent, nil
			}
		}
	}
	// This reserved endpoint is intentionally loopback and token-authenticated,
	// so opt out of sbx's SSRF guard and hosted OAuth flow for this add only.
	_, stderr, addErr := runSbxCapturedOut("mcp", "add", name, "--url", url, "--skip-ssrf-check", "--skip_auth")
	if addErr != nil {
		return provision.MCPRegistrationNone, fmt.Errorf("%w: %s", addErr, container.RedactMemoryURLToken(strings.TrimSpace(stderr)))
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
