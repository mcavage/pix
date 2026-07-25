package main

import (
	"fmt"
	"io"
	"strings"

	"pi-stack/host/config"
)

// render writes the verdict-first report to w. Default (verbose=false) is
// CONCISE: verified-ready checks collapse to a single per-group summary line
// so the output leads with what needs attention. verbose=true retains the
// full detailed group evidence, one line per check.
func (r *report) render(w io.Writer, verbose bool) {
	todos := r.todos()

	// One-line headline up front, derived entirely from requirement+verdict:
	// a VERIFIED core failure is the hard ✗ (exit 1); verified optional
	// failures are the ⚠ outstanding count; a report with nothing verified
	// failing but unverifiable checks is called out as "could not verify"
	// (never "outstanding" — there is nothing confirmed to fix).
	unv := r.unverifiableCount()
	switch {
	case r.blocking():
		fmt.Fprintf(w, "%s pi-stack: a required core check is verified failing — fix it and re-run (doctor exits 1).\n",
			verdictGlyph(requirementCore, verdictTodo, false))
	case r.outstanding() > 0:
		fmt.Fprintf(w, "%s pi-stack: %s outstanding (optional, nothing blocking) — see the TODOs below.\n",
			verdictGlyph(requirementOptional, verdictTodo, false), plural(r.outstanding(), "item"))
	case unv > 0:
		fmt.Fprintf(w, "%s pi-stack: no verified failures, but %s could not be verified from here.\n",
			verdictGlyph(requirementOptional, verdictTodo, false), plural(unv, "check"))
	default:
		fmt.Fprintf(w, "%s pi-stack: all checks pass — you're ready to `pi-stack serve` + `pi-stack`.\n",
			verdictGlyph(requirementCore, verdictReady, false))
	}
	if r.sbxAbsent {
		fmt.Fprintln(w, "  note: sbx not on PATH (you're likely inside the sandbox) — provider/MCP")
		fmt.Fprintln(w, "        checks can't be verified here; run `pi-stack doctor` on the host.")
	}
	fmt.Fprintln(w)

	// Only hint at --verbose when concise mode actually HID something. A
	// cold/all-todo run (nothing ready to collapse) shows every check already,
	// so the hint would point at detail that doesn't exist.
	collapsedAny := false
	for _, g := range r.groups {
		fmt.Fprintf(w, "%s:\n", g.title)
		shown := 0
		for _, c := range g.checks {
			if !verbose && !c.note && c.result() == verdictReady {
				collapsedAny = true
				continue // concise: collapse verified-ready detail
			}
			fmt.Fprintf(w, "  %s %-12s %s\n", glyph(c.state()), c.label, c.detail)
			shown++
		}
		if !verbose && shown == 0 {
			fmt.Fprintf(w, "  %s all %s ready\n", verdictGlyph(requirementCore, verdictReady, false), plural(len(g.checks), "check"))
		}
		fmt.Fprintln(w)
	}

	if len(todos) > 0 {
		fmt.Fprintln(w, "TODO (copy-paste, in dependency order):")
		for _, t := range todos {
			fmt.Fprintf(w, "  TODO: %s\n", t)
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "Config: %s   (services=%s, mcp=%s)\n",
		config.Path(), strings.Join(r.cfgServices(), " "), r.cfgMCP())
	// The hint is only useful — and only printed — when concise mode actually
	// hid a ready detail line; otherwise --verbose would show nothing new.
	if !verbose && collapsedAny {
		fmt.Fprintln(w, "(concise output; run `pi-stack doctor --verbose` for full group detail)")
	}
	// Security disclosure, printed only when there is something to disclose
	// (at least one MCP server configured) so a bare/no-MCP report stays
	// notice-free. Concise on purpose: full detail lives in SECURITY.md, this
	// is the reminder at the one place a user checks MCP health.
	if len(r.mcp) > 0 {
		fmt.Fprintln(w, mcpHostTrustNotice)
	}
}

// mcpHostTrustNotice is the two-fact disclosure for local command/container
// MCP servers: they run on the host, outside sandbox isolation, with your
// host-user privileges, and anything they return can end up in the
// conversation sent to your model provider. Shared verbatim by doctor's
// footer and setup's completion summary so the two surfaces never drift.
const mcpHostTrustNotice = "Note: local/container MCP servers run on the host, outside the sandbox, with your host-user privileges. Content they return can be included in the conversation sent to your model provider. Details: SECURITY.md."

// cfgServices / cfgMCP are filled by runDoctorCmd; keep them on the report so
// render stays config-free. Stored at build time.
func (r *report) cfgServices() []string { return r.services }
func (r *report) cfgMCP() string {
	if len(r.mcp) == 0 {
		return "<none>"
	}
	return strings.Join(r.mcp, " ")
}

// glyph maps a rendered checkState to its marker. It is a thin adapter over
// the shared vocabulary in readiness_render.go: doctor renders core-weight
// glyphs (its ✗ historically covered every verified failure), so the mapping
// goes through verdictGlyph with a core requirement rather than spelling the
// markers a second time.
func glyph(s checkState) string {
	switch s {
	case stateOK:
		return verdictGlyph(requirementCore, verdictReady, false)
	case stateTODO:
		return verdictGlyph(requirementCore, verdictTodo, false)
	case stateWarn:
		// An unverifiable check renders as the shared "can't check from here"
		// marker, never as a failure glyph.
		return verdictGlyph(requirementCore, verdictUnverifiable, false)
	default:
		return verdictGlyph(requirementCore, verdictReady, true)
	}
}

func upDown(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
