package envinfo

import (
	"fmt"
	"sort"
	"strings"
)

// MCPMemoryName and MCPSessionName are the two MCP server names Pix
// reserves for its own built-ins (docs/design/pix-v2-architecture.md §10,
// PRD "Reserve built-in MCP names `pix-memory` and `pix-session`"). The
// effective compiler emits both as ordinary native sbx MCP declarations;
// nothing else may claim either name.
const (
	MCPMemoryName  = "pix-memory"
	MCPSessionName = "pix-session"
)

// ReservedMCPNames returns the reserved built-in names in a stable order.
// It returns a fresh slice so a caller can never mutate the reservation.
func ReservedMCPNames() []string {
	return []string{MCPMemoryName, MCPSessionName}
}

// IsReservedMCPName reports whether name is one of Pix's built-ins.
func IsReservedMCPName(name string) bool {
	switch strings.TrimSpace(name) {
	case MCPMemoryName, MCPSessionName:
		return true
	}
	return false
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
