package envinfo

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"pix/host/stack"
)

// MCPMemoryName and MCPSessionName are Pix's two LEGACY, unscoped built-in
// MCP server names (docs/design/pix-v2-architecture.md §10, PRD "Reserve
// built-in MCP names `pix-memory` and `pix-session`"), predating per-
// PIX_HOME stack scoping (Wave A/B coexistence). They remain reserved
// forever — a host that never scoped anything, or a name an author simply
// guesses, must still refuse — but no CURRENT runtime composition ever
// emits either bare form any more: see IsMemoryMCPName/IsSessionMCPName for
// the reservation check that also covers the whole scoped family, and
// BuiltinMCPServers for what actually renders (this PIX_HOME's own
// stack.MCPMemoryName/MCPSessionName, always suffixed with its stack id,
// never a global fallback to these bare constants).
const (
	MCPMemoryName  = "pix-memory"
	MCPSessionName = "pix-session"
)

// scopedMemoryNameRe and scopedSessionNameRe match ANY valid per-stack
// scoped built-in name — "pix-memory-<16 lowercase hex>" / "pix-session-<16
// lowercase hex>" — for ANY stack id, not just this process's own. An
// authored environment must never be able to shadow another PIX_HOME's
// built-in either (the whole point of reserving the FAMILY, not one
// literal string): a name that merely happens to look like a foreign
// stack's pix-memory is exactly as reserved as this host's own.
var (
	scopedMemoryNameRe  = regexp.MustCompile(fmt.Sprintf(`^pix-memory-[0-9a-f]{%d}$`, stack.IDLen))
	scopedSessionNameRe = regexp.MustCompile(fmt.Sprintf(`^pix-session-[0-9a-f]{%d}$`, stack.IDLen))
)

// IsMemoryMCPName reports whether name is Pix's reserved pix-memory
// built-in, in either its bare legacy form or any per-stack scoped form.
func IsMemoryMCPName(name string) bool {
	n := strings.TrimSpace(name)
	return n == MCPMemoryName || scopedMemoryNameRe.MatchString(n)
}

// IsSessionMCPName reports whether name is Pix's reserved pix-session
// built-in, in either its bare legacy form or any per-stack scoped form.
func IsSessionMCPName(name string) bool {
	n := strings.TrimSpace(name)
	return n == MCPSessionName || scopedSessionNameRe.MatchString(n)
}

// ReservedMCPNames returns the two LEGACY reserved names in a stable order,
// for display purposes only (the exact rename-it wording
// ReservedMCPCollisionError prints). The full reservation IsReservedMCPName
// enforces is bigger than this list — it also covers the whole scoped
// family — because that family cannot be enumerated as a finite set of
// literal strings.
func ReservedMCPNames() []string {
	return []string{MCPMemoryName, MCPSessionName}
}

// IsReservedMCPName reports whether name is one of Pix's built-ins: either
// legacy bare name, or ANY validly-shaped per-stack scoped name in either
// family. Reserving the whole family (not merely this PIX_HOME's own
// current stack id) means an authored environment can never shadow a
// built-in this host will compose today OR a different PIX_HOME's built-in
// it might see on the same machine.
func IsReservedMCPName(name string) bool {
	return IsMemoryMCPName(name) || IsSessionMCPName(name)
}

// ReservedMCPCollisionError is the refusal for an authored `.sbxenv.yaml`
// that declares an MCP server under a reserved built-in name. Pix does not
// silently win the collision (the author would think their server was
// attached) and does not silently lose it (memory or session control would
// vanish): the launch refuses and names the exact key to rename.
type ReservedMCPCollisionError struct {
	Source string   // authored file, when known
	Names  []string // colliding reserved names, sorted
}

func (e *ReservedMCPCollisionError) Error() string {
	where := ""
	if s := strings.TrimSpace(e.Source); s != "" {
		where = " in " + s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "pix: %s%s %s reserved for a Pix built-in MCP server; refusing to launch.",
		plural(len(e.Names), "MCP server", "MCP servers"), where, plural(len(e.Names), "name is", "names are"))
	for _, n := range e.Names {
		fmt.Fprintf(&b, "\n     collides: mcp.servers[%s]", n)
	}
	fmt.Fprintf(&b, "\n     rename it: choose any name other than %s", strings.Join(ReservedMCPNames(), " or "))
	return b.String()
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// RefuseReservedMCPNames refuses an authored document that declares any
// reserved built-in MCP name. It inspects the AUTHORED document only —
// the effective compiler adds Pix's own built-ins afterwards, so running
// this check against a composed document would refuse Pix's own emission.
func RefuseReservedMCPNames(doc *Document) error {
	if doc == nil {
		return nil
	}
	var hits []string
	seen := map[string]bool{}
	for _, s := range doc.MCP.Servers {
		n := strings.TrimSpace(s.Name)
		if IsReservedMCPName(n) && !seen[n] {
			seen[n] = true
			hits = append(hits, n)
		}
	}
	if len(hits) == 0 {
		return nil
	}
	sort.Strings(hits)
	return &ReservedMCPCollisionError{Source: doc.Source, Names: hits}
}

// BuiltinMCPFacts is the caller-resolved facts needed to render Pix's two
// reserved built-in MCP declarations (docs/design/pix-v2-architecture.md
// §10): pix-memory, a remote Streamable HTTP endpoint registered by `pix
// setup`, and pix-session, a Gateway-launched host stdio command.
// MemoryName and SessionName are THIS PIX_HOME's own scoped names (stack.
// MCPMemoryName/MCPSessionName — "pix-memory-<stack id>"/"pix-session-<stack
// id>"): every current caller resolves them from its own PIX_HOME's stack
// id, never a bare legacy fallback, so BuiltinMCPServers has no global name
// of its own left to reach for. A field left at its zero value means the
// caller could not resolve that built-in yet (no memory container
// reconciled, no resolvable pix executable, no derivable stack id) —
// BuiltinMCPServers renders no entry for it rather than inventing one, and
// in particular never falls back to MCPMemoryName/MCPSessionName when a
// Name field is empty but its paired URL/Command is not: an unscoped name
// is exactly the collision this whole mechanism exists to prevent.
type BuiltinMCPFacts struct {
	MemoryName     string
	MemoryURL      string
	SessionName    string
	SessionCommand string
	SessionArgs    []string
}

// BuiltinMCPServers renders Pix's reserved built-ins, in the fixed order
// pix-memory then pix-session, as ordinary MCPWrapperFact entries. They
// need no host review of their own — RefuseReservedMCPNames is what keeps
// an authored server from ever colliding with either name in the first
// place, so by the time this runs both names are exclusively Pix's own.
func BuiltinMCPServers(facts BuiltinMCPFacts) []MCPWrapperFact {
	var out []MCPWrapperFact
	if name := strings.TrimSpace(facts.MemoryName); name != "" && strings.TrimSpace(facts.MemoryURL) != "" {
		out = append(out, MCPWrapperFact{Name: name, URL: facts.MemoryURL})
	}
	if name := strings.TrimSpace(facts.SessionName); name != "" && strings.TrimSpace(facts.SessionCommand) != "" {
		out = append(out, MCPWrapperFact{
			Name:    name,
			Command: facts.SessionCommand,
			Args:    append([]string(nil), facts.SessionArgs...),
		})
	}
	return out
}

// WithBuiltinMCPServers layers Pix's own reserved built-ins onto an
// already-composed server list, LAST, and WINNING: any existing entry
// under either reserved name is dropped first. Such an entry can only ever
// have reached this point through a host-global static-MCP NAME the
// legacy pack/config surface asked to preload (§9.2's "host-global server
// names this create preloads") — never an authored `.sbxenv.yaml` server,
// which RefuseReservedMCPNames already refused long before an effective
// document is composed. A name-only placeholder from that legacy surface
// must never shadow the real URL/command Pix's own built-in carries, so it
// is superseded here rather than folded in beside it.
func WithBuiltinMCPServers(servers []MCPWrapperFact, builtin BuiltinMCPFacts) []MCPWrapperFact {
	out := make([]MCPWrapperFact, 0, len(servers)+2)
	for _, s := range servers {
		if IsReservedMCPName(s.Name) {
			continue
		}
		out = append(out, s)
	}
	out = append(out, BuiltinMCPServers(builtin)...)
	if len(out) == 0 {
		return nil
	}
	return out
}

// UndefinedInterpolationError is the refusal for an authored `${VAR}` with
// no default and no value on the host environment. v1 resolved it to the
// empty string, which silently produced an environment nobody authored (an
// empty registry host, an empty MCP URL). PRD: "Undefined bare `${VAR}`
// interpolation is refused rather than accepted as empty."
type UndefinedInterpolationError struct {
	Vars     []string // undefined variable names, sorted, deduplicated
	KeyPaths []string // one key path per Vars entry, in the same order
}

func (e *UndefinedInterpolationError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pix: %s no value on this host and no `${VAR:-default}`; refusing to launch rather than interpolating an empty string.",
		plural(len(e.Vars), "an environment variable referenced by this environment has", "environment variables referenced by this environment have"))
	for i, v := range e.Vars {
		kp := ""
		if i < len(e.KeyPaths) {
			kp = e.KeyPaths[i]
		}
		fmt.Fprintf(&b, "\n     undefined: ${%s} at %s", v, kp)
	}
	fmt.Fprintf(&b, "\n     fix it: export the variable, or author a default as ${%s:-value}", e.Vars[0])
	return b.String()
}

// RefuseUndefinedInterpolations walks every authored interpolation surfaced
// on tree and refuses the ones that resolve to nothing: no default in the
// authored expression, and no value from lookup. An interpolation with a
// default (even an empty one) is authored intent and passes.
//
// lookup is the host environment probe (os.LookupEnv in production). A nil
// lookup means "nothing is defined", which refuses every bare reference —
// the fail-closed direction.
func RefuseUndefinedInterpolations(tree *Tree, lookup func(string) (string, bool)) error {
	if tree == nil {
		return nil
	}
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	seen := map[string]bool{}
	var vars, keys []string
	for _, in := range tree.Interpolations {
		if in.Default != nil {
			continue
		}
		if _, ok := lookup(in.Var); ok {
			continue
		}
		if seen[in.Var] {
			continue
		}
		seen[in.Var] = true
		vars = append(vars, in.Var)
		keys = append(keys, in.KeyPath)
	}
	if len(vars) == 0 {
		return nil
	}
	sortPairs(vars, keys)
	return &UndefinedInterpolationError{Vars: vars, KeyPaths: keys}
}

// sortPairs sorts vars ascending and carries keys along with them, so the
// refusal lists every undefined variable in one deterministic order.
func sortPairs(vars, keys []string) {
	idx := make([]int, len(vars))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return vars[idx[a]] < vars[idx[b]] })
	sv := make([]string, len(vars))
	sk := make([]string, len(keys))
	for i, j := range idx {
		sv[i] = vars[j]
		sk[i] = keys[j]
	}
	copy(vars, sv)
	copy(keys, sk)
}

// PIX-managed sandbox environment variables (Wave D version identity).
//
// These are the ONLY two environment facts Pix itself injects into every
// effective document. They exist so a sandbox can name the exact launcher
// build and the exact PIX_HOME stack that created it, and so a version bump
// is VISIBLE in the creation fingerprint instead of being an invisible
// difference between two otherwise identical documents.
//
// Their composed fingerprint keys ("env.PIX_LAUNCHER_VERSION",
// "env.PIX_STACK_ID") are the only env.* keys driftclass.go treats as
// recreation-safe: they are Pix-owned construction pins, exactly like the
// pinned template, so a version bump takes the existing proof-gated
// auto-recreate path while ANY other env drift (an authored variable, a
// secret destination) stays substantive and still refuses.
const (
	EnvVarLauncherVersion = "PIX_LAUNCHER_VERSION"
	EnvVarStackID         = "PIX_STACK_ID"
)

// PixManagedEnvVars is the ONE producer of the Pix-managed env block both
// the real launch (cmd/pix's runEffectiveInput) and the `pix env
// --effective` preview (workflow/env's ComputeEffective) compose, so a
// preview can never show an env block a real create would then add to.
// An unresolvable half is OMITTED rather than rendered empty: an empty
// value is a fact ("this build has no version"), and Pix does not have one
// to state.
func PixManagedEnvVars(launcherVersion, stackID string) map[string]string {
	out := map[string]string{}
	if v := strings.TrimSpace(launcherVersion); v != "" {
		out[EnvVarLauncherVersion] = v
	}
	if id := strings.TrimSpace(stackID); id != "" {
		out[EnvVarStackID] = id
	}
	return out
}
