package health

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

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
// server this is. That case FAILS CLOSED: an unclassified server is reported
// as unknown and gets no repair command, because a repair command that cannot
// work costs a user more time than silence does.
type MCPServer struct {
	Name string
	// Remote marks a server authenticated through the hosted control plane
	// (catalog and pack-remote). Only these are auth-probed.
	Remote bool
	// RegisterFix is the exact command that registers THIS server, or empty
	// when the server's kind could not be established.
	RegisterFix string
	// Undeclared marks a name in the config that no active pack provides and
	// pix does not know an endpoint for. It is not merely unregistered: even
	// if the gateway lists it, nothing here can say what it runs, and the most
	// common cause is a registration outliving the pack that created it.
	Undeclared bool
	// Command is the host binary a Command-transport server spawns, empty for
	// every other kind. A registration naming a binary that is not on PATH is
	// a server that will fail on first use while the gateway reports it ready,
	// so this is checked before anything else is believed about it.
	Command string
	// Probe is the pack-declared argv that answers "can this server actually
	// do its job". Empty means the pack declared none, which is reported as
	// unverified — never as healthy, because absence of a check is not a pass.
	Probe []string
	// Unreadable is set when the caller could not establish what any pack
	// declares (a manifest that exists but will not load). It is NOT the same
	// as Undeclared: one says "nothing provides this", the other says "we could
	// not find out", and only the first has a safe repair.
	Unreadable string
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
	// LookPath resolves a server's declared command. Injected so a test can
	// pin "this binary is missing" without touching the real PATH. Nil means
	// the real exec.LookPath.
	LookPath func(string) (string, error)
}

// lookPath is the resolver this probe should use, defaulting to the real one.
func (p MCPProbe) lookPath() func(string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath
	}
	return exec.LookPath
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

	// Per-server checks run CONCURRENTLY, each on the shared deadline. They are
	// independent — different servers, different subprocesses — and running
	// them in sequence means one slow check starves every check after it. That
	// is not hypothetical: a health probe wrapped in `op run` blocks until
	// 1Password authorizes, so a locked vault would turn one unanswerable
	// server into six, and report five gaps that were never actually checked.
	//
	// Order is preserved by index, because the report reads in config order and
	// a report that reshuffles between runs is one nobody can diff.
	findings := make([]mcpFinding, len(p.Servers))
	var wg sync.WaitGroup
	for i, s := range p.Servers {
		wg.Add(1)
		go func(i int, s MCPServer) {
			defer wg.Done()
			findings[i] = p.checkServer(ctx, bin, o.out, s)
		}(i, s)
	}
	wg.Wait()
	return p.reduce(findings)
}

// checkServer applies the three truths to one server, in the only order that
// makes sense: an unregistered server has nothing to be attached or
// authenticated.
func (p MCPProbe) checkServer(ctx context.Context, bin, listOut string, s MCPServer) mcpFinding {
	registered := mcp.McpRegEvidenceFrom(listOut, true, s.Name)

	// Undeclared comes FIRST, and it is a gap whether or not the gateway lists
	// the name. A registered-but-undeclared server is the worse case, not the
	// better one: it is a live host command nothing can vouch for, typically
	// left behind by a pack that was changed or deactivated. Reporting it as
	// "registered ✓" is exactly the lie this probe exists to stop telling.
	if s.Unreadable != "" {
		return mcpFinding{name: s.Name, unknown: true,
			note: s.Name + ": cannot tell what your pack declares (" + s.Unreadable + ")"}
	}
	if s.Undeclared {
		if registered == mcp.McpRegYes {
			return mcpFinding{name: s.Name, gap: true,
				fix:  "sbx mcp rm " + s.Name + "   # or re-activate the pack that provides it",
				note: s.Name + ": registered, but no active pack declares it — it runs a command nothing can vouch for"}
		}
		return mcpFinding{name: s.Name, gap: true,
			fix:  "pix config unset mcp " + s.Name + "   # or activate the pack that provides it",
			note: s.Name + ": in your config, but no active pack declares it"}
	}

	switch registered {
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

	// Registered. Before believing anything else about it: does the binary it
	// names still exist? A gateway lists a registration, not a working server,
	// and a `--command` pointing at a deleted binary lists exactly like a
	// healthy one.
	if s.Command != "" {
		if _, err := p.lookPath()(s.Command); err != nil {
			return mcpFinding{name: s.Name, gap: true,
				fix: s.RegisterFix,
				note: s.Name + ": registered, but its command " + strconv.Quote(s.Command) +
					" is not on PATH — it will fail on first use"}
		}
	}

	// Registered. Auth next, because an unauthenticated remote server is
	// attached and still useless.
	authenticated := false
	if s.Remote {
		a, authOut := p.checkAuth(ctx, bin, s.Name)
		switch a {
		case mcpAuthYes:
			// Real evidence of working order for a remote server: the control
			// plane says this grant is live. Recorded so the no-probe case
			// below can distinguish "nothing was checked" from "the thing that
			// can fail for this kind of server was checked, and passed".
			authenticated = true
		case mcpAuthNo:
			why := "not authenticated"
			if mcp.McpAuthExpired(authOut) {
				why = "its authorization expired"
			}
			return mcpFinding{name: s.Name, gap: true, fix: fmt.Sprintf(MCPAuthFix, s.Name),
				note: s.Name + ": registered, " + why}
		case mcpAuthUnknown:
			return mcpFinding{name: s.Name, unknown: true,
				note: s.Name + ": registered, auth not checkable from here"}
		case mcpAuthNotRequired:
			// Answered, not unproven: this server carries no OAuth, so there is
			// nothing left to establish beyond registration.
			return mcpFinding{name: s.Name, note: s.Name + ": registered, no OAuth required" + attachmentCaveat}
		}
	}

	// Everything above establishes that the server is WIRED. Whether it can
	// actually do its job is a different question, and only the server can
	// answer it — so run the probe the pack declared for exactly this.
	res, o := p.runProbe(ctx, s)
	switch res {
	case probeFailed:
		return mcpFinding{name: s.Name, gap: true, fix: s.RegisterFix,
			note: s.Name + ": registered, but its own health probe fails — see `" +
				strings.Join(s.Probe, " ") + "`"}
	case probeNotDeclared:
		if authenticated {
			// A remote server's failure mode IS its grant, and that was
			// checked. Calling this "unverified" would be its own kind of
			// dishonesty — the check that matters for this kind ran and passed.
			return mcpFinding{name: s.Name, note: s.Name + ": registered and authenticated" + attachmentCaveat}
		}
		// Not a gap and not a pass. Registration is real evidence; it is just
		// not evidence of health, and saying so is the whole point.
		return mcpFinding{name: s.Name,
			note: s.Name + ": registered; no health probe declared, so working order is unverified" + attachmentCaveat}
	case probeUnknown:
		// Say WHY this is unanswerable, because the usual cause is fixable and
		// invisible: the probe runs through `op run`, exactly as the gateway
		// will, so a locked 1Password vault stops it. That is the same thing
		// that would stop the server itself — which is the point of probing
		// this way — but a bare "could not run" sends people hunting the wrong
		// problem.
		//
		// Match the BASE NAME exactly. A suffix test here would tell the owner
		// of `hadoop` or `develop` to go unlock a vault their probe never
		// touches, which is its own small lie.
		hint, unknownFix := "", ""
		if len(s.Probe) > 0 && filepath.Base(s.Probe[0]) == "op" {
			hint = "; it runs through 1Password (`op run`), so unlock your vault and re-run"
			// A locked vault is the NORMAL state on a fresh laptop, which makes
			// this the most likely thing a new user hits — so it gets a real
			// fix line, not just an explanation buried in the evidence.
			unknownFix = "op signin   # then re-run pix doctor"
		}
		// The three unanswerable causes are genuinely different, and "did not
		// answer in time" is false for two of them: a probe binary that does
		// not exist answered instantly, it just is not there.
		return mcpFinding{name: s.Name, unknown: true, fix: unknownFix,
			note: s.Name + ": registered; " + probeUnknownReason(o) + hint}
	}

	// Registered, resolvable, (if remote) authenticated, and its own probe
	// passes. Whether a running session has the server ATTACHED is still not
	// checkable from here, so the note says exactly that rather than claiming
	// either way.
	return mcpFinding{name: s.Name, note: s.Name + ": registered and answering" + attachmentCaveat}
}

// probeResult is what a declared health probe established, kept separate from
// the finding vocabulary so "the pack declared no probe" can never be silently
// folded into "the probe passed".
type probeResult int

const (
	probeNotDeclared probeResult = iota
	probePassed
	probeFailed
	probeUnknown
)

// probeUnknownReason names WHICH way a probe failed to answer. Collapsing the
// three into one sentence sends a reader looking for a timeout that never
// happened.
func probeUnknownReason(o execOutcome) string {
	switch {
	case o.notFound:
		return "its health probe command was not found"
	case o.denied:
		return "its health probe was refused by the system"
	default:
		return "its health probe did not answer in time"
	}
}

// runProbe executes the pack-declared probe argv, bounded like every other
// check here. A probe is a READ-ONLY question a server answers about itself
// (`gog auth doctor`, a `--version`, a status subcommand); pix neither
// interprets its output nor cares what it prints, only whether it exits clean.
// That keeps the contract something a pack author can satisfy without pix
// knowing anything about their vendor.
func (p MCPProbe) runProbe(ctx context.Context, s MCPServer) (probeResult, execOutcome) {
	if len(s.Probe) == 0 {
		return probeNotDeclared, execOutcome{}
	}
	o := runBounded(ctx, s.Probe[0], s.Probe[1:]...)
	switch {
	case o.notFound || o.timedOut || o.denied:
		return probeUnknown, o
	case o.failed:
		return probeFailed, o
	}
	return probePassed, o
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
	// mcpAuthNotRequired: sbx said the server has no OAuth. An ANSWER, not a
	// gap in what we could find out.
	mcpAuthNotRequired
)

// checkAuth returns the verdict AND the raw output it was read from, so a
// caller can word the finding more precisely (expired vs never authorized)
// without running the probe a second time.
func (p MCPProbe) checkAuth(ctx context.Context, bin, name string) (mcpAuth, string) {
	o := runBounded(ctx, bin, p.authArgv(name)...)
	if o.notFound || o.timedOut {
		return mcpAuthUnknown, o.out
	}
	// A non-zero exit is NOT itself an answer: sbx exits non-zero both for
	// "you are not logged in" and for "I fell over". Only the wording decides,
	// and only when it is unambiguous.
	switch mcp.McpAuthStatus(o.out) {
	case mcp.McpAuthOK:
		return mcpAuthYes, o.out
	case mcp.McpAuthFailed:
		return mcpAuthNo, o.out
	case mcp.McpAuthNotRequired:
		return mcpAuthNotRequired, o.out
	}
	return mcpAuthUnknown, o.out
}

// reduce turns the per-server findings into the one Result the report shows.
// Precedence: a verified gap dominates (it has an exact fix), then anything
// unproven; only an all-clear reports ready.
func (p MCPProbe) reduce(findings []mcpFinding) Result {
	var gaps, unknowns int
	fix, unknownFix := "", ""
	notes := make([]string, 0, len(findings))
	for _, f := range findings {
		notes = append(notes, f.note)
		switch {
		case f.gap:
			gaps++
			if fix == "" {
				fix = f.fix
			} else if f.fix != "" && f.fix != fix {
				// More than one server needs a DIFFERENT remedy. Printing only
				// the first turns repair into a guessing loop: fix, re-run,
				// discover the next one, repeat. Collect them instead.
				fix += "\n" + f.fix
			}
		case f.unknown:
			unknowns++
			if unknownFix == "" {
				unknownFix = f.fix
			}
		}
	}
	// Newline-joined, not "; ": a note contains its own semicolons ("registered
	// (host registration; attachment ... not checkable)"), so any in-band
	// separator the renderer could split on also splits the notes themselves --
	// which is exactly what turned 8 servers into 16 half-sentences.
	ev := strings.Join(notes, "\n")
	total := len(findings)
	switch {
	case gaps > 0:
		// Report BOTH counts. Reporting only gaps hid the unknowns behind them,
		// and on the host this was built for the hidden two were the two host
		// commands the whole change is about — a reader saw "2 of 6 not usable"
		// and reasonably assumed the other four were fine.
		detail := fmt.Sprintf("%d of %d not usable", gaps, total)
		if unknowns > 0 {
			detail += fmt.Sprintf(", %d not checkable", unknowns)
		}
		if fix == "" {
			fix = unknownFix
		}
		return Result{Name: p.Name(), Status: StatusAbsent, Detail: detail, Fix: fix, Evidence: ev}
	case unknowns > 0:
		return Result{Name: p.Name(), Status: StatusUnknown, Fix: unknownFix,
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
