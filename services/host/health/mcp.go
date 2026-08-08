package health

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"pix/host/mcp"
)

// mcp.go is the MCP diagnosis on the health model. It answers each MCP truth
// SEPARATELY, because each has its own honest source and inferring one from
// another is the bug this probe exists to prevent:
//
//	registration  a bounded `sbx mcp ls`, read through mcp.McpRegEvidenceFrom
//	              — the ONE definition of "registered" in the tree.
//	auth          a bounded `sbx mcp auth status <name>`, and only for a
//	              server whose auth is hosted-control-plane OAuth. A local
//	              stdio server has no control-plane auth to check.
//
// Both are tri-state. The rule the whole probe is built around: a listing that
// did not ANSWER means unknown, never "not registered".
//
// Attachment is deliberately NOT a third truth: nothing pix can run answers
// what a live session currently has attached, and a launcher-written receipt
// is a verdict earned by a past action rather than a probe (AGENTS.md safety
// invariant #13). Do not reintroduce an attachment claim here without a live
// query that can be wrong.

// MCP fixes. Registration is per-server (the command depends on what KIND of
// server it is, which only the caller can classify), so it is carried on the
// server rather than named here.
const (
	// MCPGatewayFix is what to run when sbx is present but the MCP listing
	// itself was refused: the gateway, not any one server, is the problem.
	MCPGatewayFix = "sbx mcp status"
	// MCPAuthFix authenticates one remote server.
	MCPAuthFix = "pix mcp auth %s"
	// MCPNoneConfigured is the detail for a host that uses no MCP. It is a
	// constant because the doctor renderer reads it to decide whether the
	// host-trust disclosure applies, and a report that discloses a risk the
	// user has not taken is noise.
	MCPNoneConfigured = "none configured"
)

// MCPHostTrustNotice is the two-fact disclosure for local command/container
// MCP servers: they run on the host, OUTSIDE sandbox isolation, with the
// user's own privileges, and anything they return can end up in the
// conversation sent to the model provider. It lives next to the probe that
// knows whether any are configured, and it is printed by the doctor renderer
// whenever at least one is.
const MCPHostTrustNotice = "Note: local/container MCP servers run on the host, outside the sandbox, with your host-user privileges. Content they return can be included in the conversation sent to your model provider. Details: SECURITY.md."

// MCPServer is one configured MCP server, already CLASSIFIED by the caller.
//
// RegisterFix is empty when the caller could not establish what kind of
// server this is (the local-name set was unreadable, and it is not a
// pack/catalog name). That case FAILS CLOSED: an unclassified server is
// reported as unknown and gets no repair command, because recommending
// `pix mcp register` for a remote name — or `pix mcp bundle` for a local one
// — is a broken repair that costs a user more time than silence.
type MCPServer struct {
	Name string
	// Remote marks a server authenticated through the hosted control plane
	// (catalog and pack-remote). Only these are auth-probed.
	Remote bool
	// RegisterFix is the exact command that registers THIS server, or empty
	// when the server's kind could not be established.
	RegisterFix string
}

// MCPProbe checks every configured MCP server's registration, attachment and
// (for remote servers) auth.
type MCPProbe struct {
	// Servers is the configured set, in config order. Empty means MCP is not
	// in use on this host, which is a perfectly healthy state.
	Servers []MCPServer
	// Bin is the sbx CLI. Empty means the real one on PATH.
	Bin string
	// ListArgs and AuthArgs override the two argv the probe runs (defaults
	// `mcp ls` and `mcp auth status`, the server name appended to the
	// latter). They are the seam the tests drive: the probe still execs a
	// REAL process, just one that can be made to fail on purpose.
	ListArgs []string
	AuthArgs []string
}

func (MCPProbe) Name() string { return "mcp" }

// Required is false: MCP is opt-in, and a server that is configured but not
// yet registered is a real gap a user wants to SEE without it failing the
// exit code of every script that runs `pix doctor`.
func (MCPProbe) Required() bool { return false }

func (p MCPProbe) listArgv() []string {
	if len(p.ListArgs) > 0 {
		return p.ListArgs
	}
	return []string{"mcp", "ls"}
}

func (p MCPProbe) authArgv(name string) []string {
	if len(p.AuthArgs) > 0 {
		return append(append([]string{}, p.AuthArgs...), name)
	}
	return []string{"mcp", "auth", "status", name}
}

// mcpFinding is one server's three answers, reduced to what the report shows.
type mcpFinding struct {
	name string
	// note is the evidence fragment for this server, e.g.
	// "notion: registered, not attached".
	note string
	// gap is set when this server VERIFIED a gap; fix is its repair (possibly
	// empty, for an unclassified server — which is why gap and unknown are
	// separate booleans rather than one status).
	gap     bool
	fix     string
	unknown bool
}

func (p MCPProbe) Check(ctx context.Context) Result {
	if len(p.Servers) == 0 {
		return Result{Name: p.Name(), Status: StatusReady, Detail: MCPNoneConfigured,
			Evidence: "config lists no MCP servers"}
	}
	bin := p.Bin
	if strings.TrimSpace(bin) == "" {
		bin = "sbx"
	}
	names := p.serverNames()

	o := runBounded(ctx, bin, p.listArgv()...)
	switch {
	case o.notFound:
		// sbx is the only thing that knows what is registered. Without it we
		// know nothing about ANY of the three truths.
		return Result{Name: p.Name(), Status: StatusUnknown,
			Detail:   fmt.Sprintf("%d configured, registration not checkable from here", len(p.Servers)),
			Evidence: "sbx is not on PATH; configured: " + names}
	case o.denied:
		return Result{Name: p.Name(), Status: StatusDenied, Detail: "sbx refused the MCP listing",
			Fix: MCPGatewayFix, Evidence: "sbx mcp ls was refused; configured: " + names}
	case o.timedOut || o.failed:
		r := unknownExec(p.Name(), o, "sbx mcp ls")
		r.Detail = fmt.Sprintf("%d configured, could not list registrations", len(p.Servers))
		r.Evidence += "; configured: " + names
		return r
	}

	findings := make([]mcpFinding, 0, len(p.Servers))
	for _, s := range p.Servers {
		findings = append(findings, p.checkServer(ctx, bin, o.out, s))
	}
	return p.reduce(findings)
}

// checkServer applies the three truths to one server, in the only order that
// makes sense: an unregistered server has nothing to be attached or
// authenticated.
func (p MCPProbe) checkServer(ctx context.Context, bin, listOut string, s MCPServer) mcpFinding {
	switch mcp.McpRegEvidenceFrom(listOut, true, s.Name) {
	case mcp.McpRegNo:
		if strings.TrimSpace(s.RegisterFix) == "" {
			// Fail closed: we do not know what kind of server this is, so we
			// do not know which register command is correct.
			return mcpFinding{name: s.Name, unknown: true,
				note: s.Name + ": not registered, and its kind is unclassified (no safe repair)"}
		}
		return mcpFinding{name: s.Name, gap: true, fix: s.RegisterFix, note: s.Name + ": not registered"}
	case mcp.McpRegUnknown:
		// Unreachable while listOut came from a successful listing, but the
		// tri-state is the contract: never collapse it into a verdict.
		return mcpFinding{name: s.Name, unknown: true, note: s.Name + ": registration unknown"}
	}

	// Registered. Auth next, because an unauthenticated remote server is
	// attached and still useless.
	if s.Remote {
		switch a := p.checkAuth(ctx, bin, s.Name); a {
		case mcpAuthNo:
			return mcpFinding{name: s.Name, gap: true, fix: fmt.Sprintf(MCPAuthFix, s.Name),
				note: s.Name + ": registered, not authenticated"}
		case mcpAuthUnknown:
			return mcpFinding{name: s.Name, unknown: true,
				note: s.Name + ": registered, auth not checkable from here"}
		}
	}

	// Registered and (if remote) authenticated is everything this host can
	// establish. Whether a running session has the server ATTACHED is not
	// checkable from here, so the note says exactly that instead of claiming
	// either way — and it is not counted as unknown, because the checkable
	// facts all came back clean.
	return mcpFinding{name: s.Name, note: s.Name + ": registered" + attachmentCaveat}
}

// attachmentCaveat is the one phrase every registered server's note carries:
// registration is host state, and a session sees a server's tools only if it
// was preloaded at create or loaded live. `pix mcp ls` prints the same caveat,
// deliberately in the same words.
const attachmentCaveat = " (host registration; attachment to a live session is not checkable from here)"

// mcpAuth is the auth tri-state as this probe needs it. The PARSING is not
// redone here: mcp.McpAuthStatus is the one place that decides what sbx's
// wording means, and a second opinion about that is exactly how two surfaces
// start disagreeing about the same login.
type mcpAuth int

const (
	mcpAuthUnknown mcpAuth = iota
	mcpAuthYes
	mcpAuthNo
)

func (p MCPProbe) checkAuth(ctx context.Context, bin, name string) mcpAuth {
	o := runBounded(ctx, bin, p.authArgv(name)...)
	if o.notFound || o.timedOut {
		return mcpAuthUnknown
	}
	// A non-zero exit is NOT itself an answer: sbx exits non-zero both for
	// "you are not logged in" and for "I fell over". Only the wording decides,
	// and only when it is unambiguous.
	switch mcp.McpAuthStatus(o.out) {
	case mcp.McpAuthOK:
		return mcpAuthYes
	case mcp.McpAuthFailed:
		return mcpAuthNo
	}
	return mcpAuthUnknown
}

// reduce turns the per-server findings into the one Result the report shows.
// Precedence: a verified gap dominates (it has an exact fix), then anything
// unproven; only an all-clear reports ready.
func (p MCPProbe) reduce(findings []mcpFinding) Result {
	var gaps, unknowns int
	fix := ""
	notes := make([]string, 0, len(findings))
	for _, f := range findings {
		notes = append(notes, f.note)
		switch {
		case f.gap:
			gaps++
			if fix == "" {
				fix = f.fix
			}
		case f.unknown:
			unknowns++
		}
	}
	ev := strings.Join(notes, "; ")
	total := len(findings)
	switch {
	case gaps > 0:
		return Result{Name: p.Name(), Status: StatusAbsent,
			Detail: fmt.Sprintf("%d of %d not usable", gaps, total), Fix: fix, Evidence: ev}
	case unknowns > 0:
		return Result{Name: p.Name(), Status: StatusUnknown,
			Detail: fmt.Sprintf("%d of %d not checkable from here", unknowns, total), Evidence: ev}
	}
	return Result{Name: p.Name(), Status: StatusReady,
		Detail: fmt.Sprintf("%d registered", total), Evidence: ev}
}

func (p MCPProbe) serverNames() string {
	out := make([]string, 0, len(p.Servers))
	for _, s := range p.Servers {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
