// integration.go answers ONE question an authored environment's `mcp:`
// block invites but never itself proves: for each server it DECLARES, is it
// also REGISTERED with the sbx Gateway, and — where this environment names
// a probe — has anything actually confirmed it WORKS? Those are three
// different facts, and conflating any two of them is exactly the "registered
// is reported as working" bug docs/design/integrations-remediation.md names
// as the single most load-bearing gap in the old surface (§"1. registered
// is reported as working").
//
// Declared is free: it is just membership in BillOfMaterials.MCPServers,
// already computed by ComputeBoM from the authored document. Registered
// reuses pix/host/mcp's own registration evidence (McpRegEvidenceFrom) —
// the SAME tri-state derivation `sbx mcp ls` in this repo, so this surface
// can never disagree with `pix mcp ls` about what "registered" means for the
// same listing. Reachable runs the environment's OWN declared probe argv
// (pix.toml `[host.mcp.<name>].probe_args`) when one exists — the health-
// probe hook HostMCPFact's own doc comment already promised ("pix doctor
// runs it") but that, until this file, nothing ever executed. A server with
// no declared probe reports Reachable as StatusUnknown, never a guessed
// StatusReady: this package has no other way to know whether an MCP server
// actually works, and inventing one (a bare TCP dial, an unauthenticated
// HTTP GET) would prove reachability of a socket, not of the integration —
// exactly the false-ready shape this file exists to refuse.
package env

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/mcp"
)

// IntegrationStatus is one declared MCP server's full tri-state answer.
// Declared is always true for a value this package returns — every entry
// comes from BillOfMaterials.MCPServers, which by construction only holds
// servers the environment actually declares — but the field stays explicit
// rather than implied so a JSON consumer never has to infer "present in
// this list" as "declared".
type IntegrationStatus struct {
	Name    string
	URL     string
	Command string

	// Kind is MCPKind or ServiceKind. The two shapes share this struct
	// because they share the one surface a user reads, but they do not share
	// every field: a ServiceKind row has no Registered state at all, because
	// pix registers and starts nothing for a [[host.services]] entry.
	Kind string

	Declared bool

	// Registered is StatusReady (present in `sbx mcp ls`), StatusAbsent
	// (positively absent from it), or StatusUnknown (the listing could not
	// be obtained) — never anything else; mcp.McpRegEvidenceFrom only ever
	// answers one of those three.
	Registered       health.Status
	RegisteredDetail string

	// Reachable is StatusReady only after this environment's OWN declared
	// probe argv exited zero. StatusAbsent means the probe ran and exited
	// non-zero — a verified negative. StatusUnknown covers everything this
	// package could not positively resolve: no probe declared, the probe
	// timed out, or the probe's own binary could not be executed at all.
	Reachable       health.Status
	ReachableDetail string
}

// ProbeRunner is the injectable exec seam IntegrationStatuses' reachability
// check runs a declared probe argv through — hostenv.Env.RunTimed's own
// shape (bounded timeout, capped output, "an MCP server's declared probe is
// UNTRUSTED input" discipline already established for `sbx mcp add --help`
// detection in pix/host/mcp). A nil ProbeRunner reports every probe-bearing
// server StatusUnknown rather than skipping it silently.
type ProbeRunner func(name string, args ...string) (out string, timedOut bool, err error)

// RunnerFromEnv adapts a hostenv.Env into a ProbeRunner, through its own
// System seam — nil when env carries no host system at all, exactly the
// fail-open shape doctor/probes.go's own providerRefScan uses for an
// unwired Options.
func RunnerFromEnv(e hostenv.Env) ProbeRunner {
	if e.System == nil {
		return nil
	}
	return e.RunTimed
}

// probeArgsByServer indexes a BillOfMaterials' own pix.toml
// `[host.mcp.<name>]` annotations by the declared MCP server name they
// describe, so IntegrationStatuses can look one up per server in O(1)
// rather than re-scanning HostMCP for every entry in MCPServers.
func probeArgsByServer(hostMCP []HostMCPFact) map[string][]string {
	if len(hostMCP) == 0 {
		return nil
	}
	m := make(map[string][]string, len(hostMCP))
	for _, h := range hostMCP {
		if len(h.ProbeArgs) > 0 {
			m[h.Name] = h.ProbeArgs
		}
	}
	return m
}

// IntegrationStatuses composes the declared/registered/reachable tri-state
// for every MCP server b declares. mcpOut/mcpOK are an ALREADY-FETCHED `sbx
// mcp ls` listing (never re-run per server — the same single-snapshot
// discipline pix/host/mcp's own catalogLsEvidenceOrFailClosed uses), passed
// straight to mcp.McpRegEvidenceFrom exactly as every other registration
// answer in this repo derives it. run executes a probe argv when one is
// declared; nil means no execution seam is available, which reports every
// probe-bearing server's Reachable as Unknown rather than skipping it
// silently (an omitted row would read as "nothing to check" instead of "I
// could not check").
//
// Server order is b.MCPServers' own — ComputeBoM already sorts it by name —
// so this function's output needs no further sorting to be stable.
func IntegrationStatuses(b BillOfMaterials, mcpOut string, mcpOK bool, run ProbeRunner) []IntegrationStatus {
	if len(b.MCPServers) == 0 {
		return nil
	}
	probes := probeArgsByServer(b.HostMCP)
	out := make([]IntegrationStatus, 0, len(b.MCPServers))
	for _, s := range b.MCPServers {
		st := IntegrationStatus{Name: s.Name, Kind: MCPKind, URL: s.URL, Command: s.Command, Declared: true}
		st.Registered, st.RegisteredDetail = registeredStatus(mcpOut, mcpOK, s.Name)
		st.Reachable, st.ReachableDetail = reachableStatus(run, s.Name, probes[s.Name])
		out = append(out, st)
	}
	return out
}

func registeredStatus(mcpOut string, mcpOK bool, name string) (health.Status, string) {
	switch mcp.McpRegEvidenceFrom(mcpOut, mcpOK, name) {
	case mcp.McpRegYes:
		return health.StatusReady, "present in `sbx mcp ls`"
	case mcp.McpRegNo:
		return health.StatusAbsent, "absent from `sbx mcp ls`"
	default:
		return health.StatusUnknown, "could not list sbx MCP registrations"
	}
}

// reachableProbeTimeout bounds one declared probe argv — an environment's
// probe is untrusted input exactly like a registered MCP server's own
// command (sys.ProbeTimeout's own doc comment), so it gets the same budget
// as every other bounded probe in this repo rather than a bespoke one.
const reachableProbeTimeout = 5 * time.Second

func reachableStatus(run ProbeRunner, name string, args []string) (health.Status, string) {
	if len(args) == 0 {
		return health.StatusUnknown, "no probe declared (pix.toml [host.mcp." + name + "].probe_args)"
	}
	if run == nil {
		return health.StatusUnknown, "no host execution available to run the declared probe"
	}
	out, timedOut, err := run(args[0], args[1:]...)
	switch {
	case timedOut:
		return health.StatusUnknown, "probe timed out: " + strings.Join(args, " ")
	case err != nil:
		return health.StatusAbsent, "probe exited non-zero: " + strings.Join(args, " ")
	default:
		detail := "probe ok: " + strings.Join(args, " ")
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			detail += " (" + firstLine(trimmed) + ")"
		}
		return health.StatusReady, detail
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// --- host services -----------------------------------------------------------
//
// A `[[host.services]]` entry is the OTHER half of what an environment
// integrates with, and until this file it was absent from the one status
// surface entirely: `pix env show` reported every declared MCP server's
// declared/registered/reachable answer and said nothing at all about the
// resident loopback service the same sidecar declares — even though that
// entry is the only one carrying an explicit health endpoint (`probe`).
// "Nothing printed" reads as "nothing to report", which for a warehouse proxy
// that is not answering is the single most misleading thing this surface
// could do.
//
// Registered is deliberately EMPTY for a service rather than faked into one
// of the three MCP states: pix registers nothing here and starts nothing (an
// environment's [[host.services]] is review-and-report only), so any value
// would be a claim about a registry this row does not live in.

// ServiceKind and MCPKind tag which of the two shapes an IntegrationStatus
// describes, so a JSON consumer never has to infer it from which fields
// happen to be set.
const (
	MCPKind     = "mcp"
	ServiceKind = "service"
)

// HTTPProbe is the injectable seam a declared `probe` URL is fetched
// through — bounded, and answering the same tri-state the rest of this file
// speaks. A nil HTTPProbe reports every probe-bearing service
// StatusUnknown, never StatusReady.
type HTTPProbe func(url string) (ok bool, detail string)

// HostServiceStatuses is the declared/reachable answer for every
// `[[host.services]]` entry b declares, in b's own order.
//
// Reachability is the environment's OWN declared `probe` URL and nothing
// else. It is not a bare TCP dial and not a guess from `docker ps`: this
// package cannot run docker, and "a socket accepted" is exactly the
// false-ready shape the MCP half of this file already refuses.
func HostServiceStatuses(b BillOfMaterials, probe HTTPProbe) []IntegrationStatus {
	if len(b.HostServices) == 0 {
		return nil
	}
	out := make([]IntegrationStatus, 0, len(b.HostServices))
	for _, svc := range b.HostServices {
		st := IntegrationStatus{Name: svc.Name, Kind: ServiceKind, Command: svc.Command, Declared: true}
		st.Reachable, st.ReachableDetail = serviceReachable(probe, svc)
		out = append(out, st)
	}
	return out
}

// serviceReachable is the single definition of what "this service is up"
// means on the pix side: the URL the environment itself declared, fetched
// once, bounded.
func serviceReachable(probe HTTPProbe, svc HostServiceItem) (health.Status, string) {
	if strings.TrimSpace(svc.Probe) == "" {
		return health.StatusUnknown, "no probe declared (pix.toml [[host.services]].probe)"
	}
	if err := checkLoopbackProbe(svc.Probe); err != nil {
		return health.StatusUnknown, err.Error()
	}
	if probe == nil {
		return health.StatusUnknown, "no host execution available to run the declared probe"
	}
	ok, detail := probe(svc.Probe)
	if ok {
		return health.StatusReady, "probe ok: " + svc.Probe + suffix(detail)
	}
	return health.StatusAbsent, "probe failed: " + svc.Probe + suffix(detail)
}

func suffix(detail string) string {
	if d := strings.TrimSpace(detail); d != "" {
		return " (" + firstLine(d) + ")"
	}
	return ""
}

// checkLoopbackProbe refuses to fetch anything that is not plain HTTP on
// loopback. A `[[host.services]]` entry is a LOCAL process bound to a
// loopback port by contract — that is the only shape pix's own trust bill
// reviews it as — so an environment naming, say, https://example.com/ping
// as its "health endpoint" would turn `pix env show` into an outbound
// request made on the user's behalf, from a file whose whole job is to be
// read rather than executed. Refusing it as Unknown says so instead of
// making the request and calling the answer health.
func checkLoopbackProbe(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("probe is not a URL: " + raw)
	}
	if u.Scheme != "http" {
		return errors.New("probe is not http:// on loopback, so it was not fetched: " + raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("probe is not on loopback, so it was not fetched: " + raw)
	}
	return nil
}

// LoopbackHTTPProbe is the default HTTPProbe: one bounded GET, body
// discarded, any 2xx is up. It is deliberately not a health-body parser —
// the service decides what its own endpoint means, and this reports whether
// the service answered it.
func LoopbackHTTPProbe(url string) (bool, string) {
	client := &http.Client{Timeout: reachableProbeTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return false, err.Error()
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true, resp.Status
	}
	return false, resp.Status
}
