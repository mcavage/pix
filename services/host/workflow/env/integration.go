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
		st := IntegrationStatus{Name: s.Name, URL: s.URL, Command: s.Command, Declared: true}
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
