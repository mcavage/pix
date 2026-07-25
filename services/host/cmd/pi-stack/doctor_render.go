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
		fmt.Fprintln(w, "✗ pi-stack: a required core check is verified failing — fix it and re-run (doctor exits 1).")
	case r.outstanding() > 0:
		fmt.Fprintf(w, "⚠ pi-stack: %s outstanding (optional, nothing blocking) — see the TODOs below.\n", plural(r.outstanding(), "item"))
	case unv > 0:
		fmt.Fprintf(w, "⚠ pi-stack: no verified failures, but %s could not be verified from here.\n", plural(unv, "check"))
	default:
		fmt.Fprintln(w, "✓ pi-stack: all checks pass — you're ready to `pi-stack serve` + `pi-stack`.")
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
			fmt.Fprintf(w, "  ✓ all %s ready\n", plural(len(g.checks), "check"))
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
}

// cfgServices / cfgMCP are filled by runDoctorCmd; keep them on the report so
// render stays config-free. Stored at build time.
func (r *report) cfgServices() []string { return r.services }
func (r *report) cfgMCP() string {
	if len(r.mcp) == 0 {
		return "<none>"
	}
	return strings.Join(r.mcp, " ")
}

// glyph maps a rendered checkState to its marker: ✓ verified ready, ✗ a
// verified todo/denied, ⚠ unverifiable, · an annotation.
func glyph(s checkState) string {
	switch s {
	case stateOK:
		return "✓"
	case stateTODO:
		return "✗"
	case stateWarn:
		return "⚠"
	default:
		return "·"
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
