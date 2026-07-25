package main

import (
	"sort"
	"strings"
	"time"
)

// readiness_snapshot.go is the ONE readiness truth every command renders.
//
// The vocabulary (requirement/verdict/check) lives in readiness_types.go; this
// file adds the three things that make it shareable across doctor, status,
// run, setup and the onboarding host-state payload:
//
//  1. Axis — a frozen, machine-stable name for each thing readiness can be
//     asserted about. A renderer keys off the axis, never off a group title or
//     a human label.
//  2. Request — what THIS invocation actually asked for. A snapshot is LAZY:
//     an axis nobody requested is never probed and is ABSENT from the snapshot
//     (never rendered ready, never rendered as a verdict). That is what keeps
//     `status` cheap while `doctor` probes everything.
//  3. ExitCode — one derivation of the process exit code, so two commands
//     looking at the same facts can never disagree about whether the host is
//     ready.

// Axis is the stable identity of one readiness fact. The set is FROZEN
// (readinessAxes below): adding one is a product decision recorded in the PRD,
// not a commit, and readiness_axisfreeze_test.go fails the build on any
// addition. `mcp:<server>` is the one parameterized family.
type Axis string

const (
	axisProviders        Axis = "providers"
	axisSecrets          Axis = "secrets"
	axisSbx              Axis = "sbx"
	axisOllamaHost       Axis = "ollama.host"
	axisOllamaSandbox    Axis = "ollama.sandbox"
	axisModelWatcher     Axis = "model.watcher"
	axisModelEmbed       Axis = "model.embed"
	axisModelBridge      Axis = "model.bridge"
	axisServiceMemory    Axis = "service.memory"
	axisServiceKnowledge Axis = "service.knowledge"
	axisPack             Axis = "pack"
	axisGworkspace       Axis = "gworkspace"

	// axisMCPPrefix is the one parameterized axis family: `mcp:<server>`, one
	// per configured MCP server.
	axisMCPPrefix = "mcp:"
)

// readinessAxes is the frozen axis set, in canonical render order. The
// parameterized `mcp:<server>` family is not listed: it is covered by
// mcpAxis/Axis.known.
var readinessAxes = []Axis{
	axisProviders,
	axisSecrets,
	axisSbx,
	axisOllamaHost,
	axisOllamaSandbox,
	axisModelWatcher,
	axisModelEmbed,
	axisModelBridge,
	axisServiceMemory,
	axisServiceKnowledge,
	axisPack,
	axisGworkspace,
}

// mcpAxis names the readiness axis for one MCP server.
func mcpAxis(server string) Axis { return Axis(axisMCPPrefix + server) }

// mcpAxisServer returns the server name behind an `mcp:<server>` axis.
func mcpAxisServer(a Axis) (string, bool) {
	s := string(a)
	if !strings.HasPrefix(s, axisMCPPrefix) {
		return "", false
	}
	return strings.TrimPrefix(s, axisMCPPrefix), true
}

// known reports whether a is in the frozen set (or a well-formed member of the
// parameterized mcp family). An unknown axis is a programming error: it can
// never be requested, so its builder can never run.
func (a Axis) known() bool {
	if server, ok := mcpAxisServer(a); ok {
		return strings.TrimSpace(server) != ""
	}
	for _, k := range readinessAxes {
		if k == a {
			return true
		}
	}
	return false
}

// Request is what an invocation asks the readiness layer for. Axes is the set
// to BUILD (anything else is never probed and stays absent); Requested is the
// subset the user explicitly asked for on THIS invocation, which promotes an
// optional axis to blocking — `--pull-models` promotes the model axes,
// `--google-workspace` promotes gworkspace, `--mcp X` promotes `mcp:X`.
//
// Promotion lives HERE, in the type, so no command re-implements it in its
// flag handling.
type Request struct {
	Axes      []Axis
	Requested []Axis
}

// requestAll builds a Request over every frozen axis plus one `mcp:<server>`
// axis per configured server — what `doctor` and `setup` ask for.
func requestAll(mcpServers []string, requested ...Axis) Request {
	axes := append([]Axis(nil), readinessAxes...)
	for _, s := range mcpServers {
		if strings.TrimSpace(s) == "" {
			continue
		}
		axes = append(axes, mcpAxis(s))
	}
	return Request{Axes: axes, Requested: requested}
}

func (r Request) wants(a Axis) bool {
	for _, x := range r.Axes {
		if x == a {
			return true
		}
	}
	return false
}

// promoted reports whether a was explicitly requested on this invocation, in
// which case its optional checks block like core ones.
func (r Request) promoted(a Axis) bool {
	for _, x := range r.Requested {
		if x == a {
			return true
		}
	}
	return false
}

// axisBuilder produces the checks for one axis. It is only ever called when
// the axis was requested (laziness), and at most once per snapshot.
type axisBuilder func() []check

// Snapshot is the built readiness truth for one invocation: the axes that were
// requested, each with its checks. Absent axes are ABSENT — a renderer that
// asks for one gets (nil, false), never a fabricated verdict.
type Snapshot struct {
	order  []Axis
	checks map[Axis][]check
}

// buildSnapshot runs the builders for exactly the requested axes, in the order
// the request listed them, and applies `requested` promotion to the resulting
// checks. Builders for unrequested (or unknown) axes are never called: that is
// the whole cost model behind "status stays fast".
func buildSnapshot(req Request, builders map[Axis]axisBuilder) Snapshot {
	s := Snapshot{checks: map[Axis][]check{}}
	for _, a := range req.Axes {
		if !a.known() {
			continue
		}
		if _, done := s.checks[a]; done {
			continue
		}
		b := builders[a]
		if b == nil {
			continue
		}
		start := time.Now()
		got := b()
		elapsed := time.Since(start)
		out := make([]check, 0, len(got))
		for _, c := range got {
			c.axis = a
			if c.duration == 0 {
				c.duration = elapsed
			}
			if req.promoted(a) && c.req() == requirementOptional && !c.note {
				// Promotion lives in the type: an optional axis the user
				// explicitly asked for blocks like core, for this invocation
				// only. Notes never block, so they are never promoted.
				c.requirement = requirementRequested
			}
			out = append(out, c)
		}
		s.order = append(s.order, a)
		s.checks[a] = out
	}
	return s
}

// Axes returns the built axes in request order.
func (s Snapshot) Axes() []Axis { return append([]Axis(nil), s.order...) }

// Has reports whether the axis was requested and built.
func (s Snapshot) Has(a Axis) bool { _, ok := s.checks[a]; return ok }

// Checks returns one axis's checks, and whether the axis is present at all.
func (s Snapshot) Checks(a Axis) ([]check, bool) {
	c, ok := s.checks[a]
	return c, ok
}

// All returns every check in the snapshot, in axis order.
func (s Snapshot) All() []check {
	var out []check
	for _, a := range s.order {
		out = append(out, s.checks[a]...)
	}
	return out
}

// verdictRank orders verdicts by how much attention they demand, so an axis
// with several checks reports its WORST one: a verified failure outranks an
// unverifiable, which outranks ready.
func verdictRank(v verdict) int {
	switch v {
	case verdictTodo, verdictDenied:
		return 3
	case verdictUnverifiable:
		return 2
	default:
		return 1
	}
}

// AxisVerdict reduces one axis to a single (requirement, verdict) pair — the
// worst non-note check on it, with the strongest requirement seen. Absent
// axes report ok=false; a renderer must show nothing rather than invent a row.
func (s Snapshot) AxisVerdict(a Axis) (requirement, verdict, bool) {
	checks, ok := s.checks[a]
	if !ok {
		return "", "", false
	}
	req := requirementOptional
	v := verdictReady
	seen := false
	for _, c := range checks {
		if c.note {
			continue
		}
		seen = true
		if requirementRank(c.req()) > requirementRank(req) {
			req = c.req()
		}
		if verdictRank(c.result()) > verdictRank(v) {
			v = c.result()
		}
	}
	if !seen {
		// Note-only axis: it was probed, but nothing was asserted.
		return requirementOptional, verdictUnverifiable, true
	}
	return req, v, true
}

// requirementRank orders requirements by blocking strength.
func requirementRank(r requirement) int {
	switch r {
	case requirementCore:
		return 3
	case requirementRequested:
		return 2
	default:
		return 1
	}
}

// Exit codes. This is the ONE contract, shared by every readiness command:
//
//	0 — every core and requested axis is ready
//	1 — at least one core/requested axis is a VERIFIED failure (todo/denied)
//	2 — usage error; produced by argument parsing only, before any probe or
//	    mutation, so it can never be derived from a snapshot
//	3 — at least one core/requested axis is unverifiable and none verified-failed
//
// Precedence is 2 > 1 > 3 > 0: a verified failure outranks an unverifiable.
const (
	exitReady        = 0
	exitNotReady     = 1
	exitUsage        = 2
	exitUnverifiable = 3
)

// ExitCode derives the process exit code from the snapshot. Notes never count.
// Optional axes never block; `requested` ones block exactly like core, for
// this invocation only.
func (s Snapshot) ExitCode() int {
	failed, unverifiable := false, false
	for _, c := range s.All() {
		if c.note {
			continue
		}
		if !blocksExit(c.req()) {
			continue
		}
		switch c.result() {
		case verdictTodo, verdictDenied:
			failed = true
		case verdictUnverifiable:
			unverifiable = true
		}
	}
	switch {
	case failed:
		return exitNotReady
	case unverifiable:
		return exitUnverifiable
	default:
		return exitReady
	}
}

// ExitCodeSuppressingUnverifiable is the contract with the 3 arm suppressed,
// for the two commands where an unverifiable axis must not fail the process:
// `status` (a script fetching JSON must never fail on "can't check from here")
// and `run` (launch success dominates; readiness renders as warnings).
func (s Snapshot) ExitCodeSuppressingUnverifiable() int {
	if code := s.ExitCode(); code == exitNotReady {
		return exitNotReady
	}
	return exitReady
}

// blocksExit reports whether a requirement participates in the exit code.
func blocksExit(r requirement) bool {
	return r == requirementCore || r == requirementRequested
}

// Outstanding counts verified failures across the snapshot (notes excluded) —
// the tally every headline renders.
func (s Snapshot) Outstanding() int {
	n := 0
	for _, c := range s.All() {
		if v := c.result(); !c.note && (v == verdictTodo || v == verdictDenied) {
			n++
		}
	}
	return n
}

// UnverifiableCount counts checks that could not be verified (notes excluded).
func (s Snapshot) UnverifiableCount() int {
	n := 0
	for _, c := range s.All() {
		if !c.note && c.result() == verdictUnverifiable {
			n++
		}
	}
	return n
}

// axisNames renders a snapshot's axes as sorted strings — test and JSON help.
func axisNames(axes []Axis) []string {
	out := make([]string, 0, len(axes))
	for _, a := range axes {
		out = append(out, string(a))
	}
	sort.Strings(out)
	return out
}
