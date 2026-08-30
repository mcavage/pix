// homeadapters.go — the concrete, production (non-fake) adapters the v2
// pixhome-based verbs (doctor, reset, setup) need to reach the real world:
// an os/exec-backed health.ExecChecker, an HTTP-backed container.Prober, and
// an `sbx mcp`-backed registrar/lister for the reserved pix-memory remote.
//
// KNOWN LIMITATION, stated once here rather than re-discovered per caller:
// `sbx mcp ls` has no machine-readable output (`--json`/`-o json` are both
// rejected — see mcp.RegisteredNamesFrom's own doc comment), so this host can
// only conservatively confirm a server NAME is present in the human table. It
// cannot recover the URL that name is registered at. That means
// sbxMemoryMCP can answer "not registered" with confidence but can never
// positively confirm a URL match — the architecture's promised
// "same-name mismatch refuses launch" drift check is therefore NOT
// implemented here: doing so honestly would require a real drift signal this
// host does not have. sbxMemoryMCP is wired only into `pix setup`'s
// best-effort registration step (register once if absent; leave alone if the
// name already exists, however it is pointed), and it is deliberately NOT
// wired into `pix doctor`'s MCPLister seam — an unconfigured lister there
// reports StatusUnknown ("could not verify"), which is the honest answer
// given what this host can actually observe.
package main

import (
	"bytes"
	"net/http"
	"os/exec"
	"time"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/mcp"
	"pix/host/pixhome"
	"pix/host/release"
)

// memoryHostPort is the fixed loopback port pix-memory publishes on. A
// future release manifest field may make this allocated/configurable; until
// then it is the one port every home-adapter caller (doctor, setup) agrees
// on, so a probe and a registration always describe the SAME endpoint.
const memoryHostPort = 18080

// homeContainerSpec builds the container.Spec every v2 home caller
// reconciles/probes against: the release-pinned pix-memory image (when a
// release manifest is recorded; "" otherwise, which container.Inspect and
// the doctor probes both already treat as "nothing to compare against" and
// report absent/no-manifest rather than crashing on) and this host's fixed
// loopback port and data directory.
func homeContainerSpec(home pixhome.Paths) container.Spec {
	image := ""
	if m, err := release.LoadInstalled(home.Home); err == nil && m != nil {
		image = "pix-memory@" + m.PixMemoryDigest
	}
	return container.Spec{
		ContainerName: container.Name,
		Image:         image,
		HostPort:      memoryHostPort,
		DataDir:       home.StateMemory,
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
// (the same commands `pix mcp` itself would run), never a second client.
// See this file's own doc comment for the URL-visibility limitation that
// keeps this a register-once-if-absent adapter rather than a drift check.
type sbxMemoryRegistrar struct{}

// EnsureMemoryRemote registers name at url with the sbx Gateway if it is not
// already present in `sbx mcp ls`'s listing. An existing registration under
// name is left untouched (matched reports true, since this host cannot prove
// otherwise) rather than ever being silently overwritten — the one
// unconditional rule architecture §10 states regardless of what this host
// can observe about the existing entry.
func (sbxMemoryRegistrar) EnsureMemoryRemote(name, url string) (matched bool, err error) {
	lsOut, _, lsErr := runSbxCapturedOut("mcp", "ls")
	if lsErr == nil {
		for _, n := range mcp.RegisteredNamesFrom(lsOut) {
			if n == name {
				// Already registered under this name. Cannot verify its URL from
				// this host (see the file doc comment); never overwritten
				// automatically.
				return true, nil
			}
		}
	}
	if _, _, addErr := runSbxCapturedOut("mcp", "add", name, "--url", url); addErr != nil {
		return false, addErr
	}
	return true, nil
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
