package readiness

// report_render.go renders a Report for a human.
//
// Hints is why this is not a layering violation. A renderer that hard-codes
// "brew install docker/tap/sbx@nightly" has taken an opinion about a tool it
// does not own; passing the surface's own words in keeps readiness about the
// MODEL of readiness and lets doctor, status and setup each say what they mean.

import (
	"fmt"
	"io"
	"strings"

	"pix/host/cli"
	"pix/host/config"
)

// Hints are the surface-specific strings a Report cannot know for itself.
type Hints struct {
	// SbxInstall is the exact command that installs the sandbox runtime.
	SbxInstall string
	// MCPHostTrust is the security notice shown when host MCP servers are in
	// play. It is the caller's text because the caller owns the disclosure.
	MCPHostTrust string
}

// render writes the verdict-first report to w. Default (verbose=false) is
// CONCISE: verified-ready checks collapse to a single per-group summary line
// so the output leads with what needs attention. verbose=true retains the
// full detailed group evidence, one line per check.
func (r *Report) Render(w io.Writer, verbose bool, h Hints) {
	Todos := r.Todos()

	// One-line headline up front, derived entirely from requirement+verdict:
	// a VERIFIED core failure is the hard ✗ (exit 1); verified optional
	// failures are the ⚠ Outstanding count; a report with nothing verified
	// failing but unverifiable checks is called out as "could not verify"
	// (never "outstanding" — there is nothing confirmed to fix).
	unv := r.UnverifiableCount()
	switch {
	case r.Blocking():
		fmt.Fprintf(w, "%s pix: a required core check is verified failing — fix it and re-run (doctor exits 1).\n",
			VerdictGlyph(RequirementCore, VerdictTodo, false))
	case r.Outstanding() > 0:
		fmt.Fprintf(w, "%s pix: %s outstanding (optional, nothing blocking) — see the TODOs below.\n",
			VerdictGlyph(RequirementOptional, VerdictTodo, false), cli.Plural(r.Outstanding(), "item"))
	case unv > 0:
		fmt.Fprintf(w, "%s pix: no verified failures, but %s could not be verified from here.\n",
			VerdictGlyph(RequirementOptional, VerdictTodo, false), cli.Plural(unv, "check"))
	default:
		fmt.Fprintf(w, "%s pix: all checks pass — you're ready to `pix serve` + `pix`.\n",
			VerdictGlyph(RequirementCore, VerdictReady, false))
	}
	if r.SbxAbsent {
		fmt.Fprintln(w, "  note: sbx not on PATH (you're likely inside the sandbox) — provider/MCP")
		fmt.Fprintln(w, "        checks can't be verified here; run `pix doctor` on the host.")
		fmt.Fprintln(w, "        On a macOS host, install it with: "+h.SbxInstall)
	}
	fmt.Fprintln(w)

	// Only hint at --verbose when concise mode actually HID something. A
	// cold/all-todo run (nothing ready to collapse) shows every check already,
	// so the hint would point at detail that doesn't exist.
	collapsedAny := false
	for _, g := range r.Groups {
		fmt.Fprintf(w, "%s:\n", g.Title)
		shown := 0
		for _, c := range g.Checks {
			if !verbose && !c.Note && c.Result() == VerdictReady {
				collapsedAny = true
				continue // concise: collapse verified-ready detail
			}
			fmt.Fprintf(w, "  %s %-12s %s\n", Glyph(c), c.Label, c.Detail)
			shown++
		}
		if !verbose && shown == 0 {
			fmt.Fprintf(w, "  %s all %s ready\n", VerdictGlyph(RequirementCore, VerdictReady, false), cli.Plural(len(g.Checks), "check"))
		}
		fmt.Fprintln(w)
	}

	if len(Todos) > 0 {
		fmt.Fprintln(w, "TODO (copy-paste, in dependency order):")
		for _, t := range Todos {
			fmt.Fprintf(w, "  TODO: %s\n", t)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Config: %s   (services=%s, mcp=%s)\n",
		config.Path(), joinOrNone(r.Services), joinOrNone(r.MCP))
	// The hint is only useful — and only printed — when concise mode actually
	// hid a ready detail line; otherwise --verbose would show nothing new.
	if !verbose && collapsedAny {
		fmt.Fprintln(w, "(concise output; run `pix doctor --verbose` for full group detail)")
	}
	// Security disclosure, printed only when there is something to disclose
	// (at least one MCP server configured) so a bare/no-MCP report stays
	// notice-free. Concise on purpose: full detail lives in SECURITY.md, this
	// is the reminder at the one place a user checks MCP health.
	if len(r.MCP) > 0 {
		fmt.Fprintln(w, h.MCPHostTrust)
	}
}

// joinOrNone renders a configured set, or "<none>". The empty case is spelled
// out because a blank after "mcp=" reads as a rendering bug rather than as
// "you have configured no MCP servers".
func joinOrNone(v []string) string {
	if len(v) == 0 {
		return "<none>"
	}
	return strings.Join(v, " ")
}
