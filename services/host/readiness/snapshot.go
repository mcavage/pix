package readiness

import (
	"sort"
	"strings"
	"time"
)

// readiness_snapshot.go is the ONE readiness truth every command renders.
//
// The vocabulary (Requirement/Verdict/Check) lives in readiness_types.go; this
// file adds the three things that make it shareable across doctor, status,
// run, setup and the onboarding host-state payload:
//
//  1. Axis — a frozen, machine-stable name for each thing readiness can be
//     asserted about. A renderer keys off the axis, never off a Group title or
//     a human label.
//  2. Request — what THIS invocation actually asked for. A snapshot is LAZY:
//     an axis nobody requested is never probed and is ABSENT from the snapshot
//     (never rendered ready, never rendered as a Verdict). That is what keeps
//     `status` cheap while `doctor` probes everything.
//  3. ExitCode — one derivation of the process exit code, so two commands
//     looking at the same facts can never disagree about whether the host is
//     ready.

// Axis is the stable identity of one readiness fact. The set is FROZEN
// (AllAxes below): adding one is a product decision recorded in the PRD,
// not a commit, and readiness_axisfreeze_test.go fails the build on any
// addition. `mcp:<server>` is the one parameterized family.
type Axis string

const (
	AxisProviders        Axis = "providers"
	AxisSecrets          Axis = "secrets"
	AxisSbx              Axis = "sbx"
	AxisOllamaHost       Axis = "ollama.host"
	AxisOllamaSandbox    Axis = "ollama.sandbox"
	AxisModelWatcher     Axis = "model.watcher"
	AxisModelEmbed       Axis = "model.embed"
	AxisModelBridge      Axis = "model.bridge"
	AxisServiceMemory    Axis = "service.memory"
	AxisServiceKnowledge Axis = "service.knowledge"
	AxisPack             Axis = "pack"
	AxisGworkspace       Axis = "gworkspace"

	// AxisMCPPrefix is the one parameterized axis family: `mcp:<server>`, one
	// per configured MCP server.
	AxisMCPPrefix = "mcp:"
)

// AllAxes is the frozen axis set, in canonical render order. The
// parameterized `mcp:<server>` family is not listed: it is covered by
// MCPAxis/Axis.known.
var AllAxes = []Axis{
	AxisProviders,
	AxisSecrets,
	AxisSbx,
	AxisOllamaHost,
	AxisOllamaSandbox,
	AxisModelWatcher,
	AxisModelEmbed,
	AxisModelBridge,
	AxisServiceMemory,
	AxisServiceKnowledge,
	AxisPack,
	AxisGworkspace,
}

// MCPAxis names the readiness axis for one MCP server.
func MCPAxis(server string) Axis { return Axis(AxisMCPPrefix + server) }

// MCPAxisServer returns the server name behind an `mcp:<server>` axis.
func MCPAxisServer(a Axis) (string, bool) {
	s := string(a)
	if !strings.HasPrefix(s, AxisMCPPrefix) {
		return "", false
	}
	return strings.TrimPrefix(s, AxisMCPPrefix), true
}

// known reports whether a is in the frozen set (or a well-formed member of the
// parameterized mcp family). An unknown axis is a programming error: it can
// never be requested, so its builder can never run.
func (a Axis) Known() bool {
	if server, ok := MCPAxisServer(a); ok {
		return strings.TrimSpace(server) != ""
	}
	for _, k := range AllAxes {
		if k == a {
			return true
		}
	}
	return false
}

// Request is what an invocation asks the readiness layer for. Axes is the set
// to BUILD (anything else is never probed and stays absent); Requested is the
// subset the user explicitly asked for on THIS invocation, which promotes an
// optional axis to Blocking — `--pull-models` promotes the model axes,
// `--google-workspace` promotes gworkspace, `--mcp X` promotes `mcp:X`.
//
// Promotion lives HERE, in the type, so no command re-implements it in its
// flag handling.
type Request struct {
	Axes      []Axis
	Requested []Axis
}

// RequestAll builds a Request over every frozen axis plus one `mcp:<server>`
// axis per configured server — what `doctor` and `setup` ask for.
func RequestAll(mcpServers []string, requested ...Axis) Request {
	axes := append([]Axis(nil), AllAxes...)
	for _, s := range mcpServers {
		if strings.TrimSpace(s) == "" {
			continue
		}
		axes = append(axes, MCPAxis(s))
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

// AxisBuilder produces the checks for one axis. It is only ever called when
// the axis was requested (laziness), and at most once per snapshot.
type AxisBuilder func() []Check

// Snapshot is the built readiness truth for one invocation: the axes that were
// requested, each with its checks. Absent axes are ABSENT — a renderer that
// asks for one gets (nil, false), never a fabricated Verdict.
type Snapshot struct {
	order  []Axis
	checks map[Axis][]Check
}

// Build runs the builders for exactly the requested axes, in the order
// the request listed them, and applies `requested` promotion to the resulting
// checks. Builders for unrequested (or unknown) axes are never called: that is
// the whole cost model behind "status stays fast".
func Build(req Request, builders map[Axis]AxisBuilder) Snapshot {
	s := Snapshot{checks: map[Axis][]Check{}}
	for _, a := range req.Axes {
		if !a.Known() {
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
		out := make([]Check, 0, len(got))
		for _, c := range got {
			c.Axis = a
			if c.Duration == 0 {
				c.Duration = elapsed
			}
			if req.promoted(a) && c.Req() == RequirementOptional && !c.Note {
				// Promotion lives in the type: an optional axis the user
				// explicitly asked for blocks like core, for this invocation
				// only. Notes never block, so they are never promoted.
				c.Requirement = RequirementRequested
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
func (s Snapshot) Checks(a Axis) ([]Check, bool) {
	c, ok := s.checks[a]
	return c, ok
}

// All returns every Check in the snapshot, in axis order.
func (s Snapshot) All() []Check {
	var out []Check
	for _, a := range s.order {
		out = append(out, s.checks[a]...)
	}
	return out
}

// verdictRank orders verdicts by how much attention they demand, so an axis
// with several checks reports its WORST one: a verified failure outranks an
// unverifiable, which outranks ready.
func verdictRank(v Verdict) int {
	switch v {
	case VerdictTodo, VerdictDenied:
		return 3
	case VerdictUnverifiable:
		return 2
	default:
		return 1
	}
}

// AxisVerdict reduces one axis to a single (Requirement, Verdict) pair — the
// worst non-note Check on it, with the strongest Requirement seen. Absent
// axes Report ok=false; a renderer must show nothing rather than invent a row.
func (s Snapshot) AxisVerdict(a Axis) (Requirement, Verdict, bool) {
	checks, ok := s.checks[a]
	if !ok {
		return "", "", false
	}
	req := RequirementOptional
	v := VerdictReady
	seen := false
	for _, c := range checks {
		if c.Note {
			continue
		}
		seen = true
		if requirementRank(c.Req()) > requirementRank(req) {
			req = c.Req()
		}
		if verdictRank(c.Result()) > verdictRank(v) {
			v = c.Result()
		}
	}
	if !seen {
		// Note-only axis: it was probed, but nothing was asserted.
		return RequirementOptional, VerdictUnverifiable, true
	}
	return req, v, true
}

// requirementRank orders requirements by Blocking strength.
func requirementRank(r Requirement) int {
	switch r {
	case RequirementCore:
		return 3
	case RequirementRequested:
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
	ExitReady        = 0
	ExitNotReady     = 1
	ExitUsage        = 2
	ExitUnverifiable = 3
)

// ExitCode derives the process exit code from the snapshot. Notes never count.
// Optional axes never block; `requested` ones block exactly like core, for
// this invocation only.
func (s Snapshot) ExitCode() int {
	failed, unverifiable := false, false
	for _, c := range s.All() {
		if c.Note {
			continue
		}
		if !BlocksExit(c.Req()) {
			continue
		}
		switch c.Result() {
		case VerdictTodo, VerdictDenied:
			failed = true
		case VerdictUnverifiable:
			unverifiable = true
		}
	}
	switch {
	case failed:
		return ExitNotReady
	case unverifiable:
		return ExitUnverifiable
	default:
		return ExitReady
	}
}

// RequestedShortfall returns the axes this invocation explicitly REQUESTED
// that did not end `ready`, in snapshot order.
//
// This is the promotion rule's other half (AC-P0-209/210). `requested` already
// makes an optional axis block like core through BlocksExit, but that only
// covers a VERIFIED failure: an axis the user asked for and that could not be
// checked at all (`ollama` down, so nothing can be pulled or proven) is exit 3
// under the general contract, which is the right answer for a diagnostic and
// the wrong one for a request. When the user says `--pull-models`, "I could not
// check" is a failed request, not a shrug — so a command that ASKED for an axis
// consults this and exits 1.
//
// A requested axis with no builder (nothing in this process can speak to it) is
// ABSENT from the snapshot and is NOT reported here: absence is not evidence of
// failure, and inventing one would be the exact false Verdict the snapshot model
// exists to prevent.
func (s Snapshot) RequestedShortfall(req Request) []Axis {
	var out []Axis
	for _, a := range s.order {
		if !req.promoted(a) {
			continue
		}
		if _, v, ok := s.AxisVerdict(a); ok && v != VerdictReady {
			out = append(out, a)
		}
	}
	return out
}

// ExitCodeSuppressingUnverifiable is the contract with the 3 arm suppressed,
// for the two commands where an unverifiable axis must not fail the process:
// `status` (a script fetching JSON must never fail on "can't check from here")
// and `run` (launch success dominates; readiness renders as warnings).
func (s Snapshot) ExitCodeSuppressingUnverifiable() int {
	if code := s.ExitCode(); code == ExitNotReady {
		return ExitNotReady
	}
	return ExitReady
}

// BlocksExit reports whether a Requirement participates in the exit code.
func BlocksExit(r Requirement) bool {
	return r == RequirementCore || r == RequirementRequested
}

// Outstanding counts verified failures across the snapshot (notes excluded) —
// the tally every headline renders.
func (s Snapshot) Outstanding() int {
	n := 0
	for _, c := range s.All() {
		if v := c.Result(); !c.Note && (v == VerdictTodo || v == VerdictDenied) {
			n++
		}
	}
	return n
}

// UnverifiableCount counts checks that could not be verified (notes excluded).
func (s Snapshot) UnverifiableCount() int {
	n := 0
	for _, c := range s.All() {
		if !c.Note && c.Result() == VerdictUnverifiable {
			n++
		}
	}
	return n
}

// AxisNames renders a snapshot's axes as sorted strings — test and JSON help.
func AxisNames(axes []Axis) []string {
	out := make([]string, 0, len(axes))
	for _, a := range axes {
		out = append(out, string(a))
	}
	sort.Strings(out)
	return out
}
