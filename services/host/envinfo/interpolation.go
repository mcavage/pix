package envinfo

import (
	"regexp"
	"strings"
)

// PixHomeVar is the ONE variable Pix itself defines for an authored
// `${VAR}` reference. Everything else in an environment file is a HOST
// variable: surfaced, refused when undefined, and resolved by sbx. This
// one is different in both directions, and both differences are the
// mechanism:
//
//   - It is always DEFINED. Pix resolves its own home from $PIX_HOME or
//     ~/.pix (pixhome.Dir) and never requires a shell to have exported it,
//     so `${PIX_HOME}` can never be the undefined-variable refusal
//     RefuseUndefinedInterpolations exists to raise. A person who has
//     never heard of the variable still gets the same path Pix uses.
//   - It is resolved BY PIX, before the effective document is rendered,
//     for local MCP argv only (ExpandPixManaged, called from the one
//     place each launch and preview composes MCPWrapperFact). Upstream's
//     observed interpolation results cover `env:` values only
//     (docs/upstream/sbx-0.39-environments.md §6); no run has observed
//     sbx expanding `${VAR}` inside `mcp.servers[].args`, and a literal
//     `${PIX_HOME}` reaching `docker run -v` would create a directory
//     named after the expression. Pix resolves what Pix can state.
//
// Why it must exist at all: `mcp.servers[].args` is static argv with no
// shell, so an environment that wants a container to keep state in a
// per-PIX_HOME directory has no way to name one. The alternatives are a
// hard-coded /Users/<someone> (one person's machine, committed for
// everyone), a named Docker volume (invisible to `pix reset`, and outside
// the home whose whole job is holding this stack's state), or a wrapper
// script (which hides the argv the trust bill exists to show). This
// variable is the only one of the four that keeps the real argv reviewable.
const PixHomeVar = "PIX_HOME"

// PixManagedVars is the complete set of Pix-defined interpolation
// variables for one resolved home. It is a map, not a single string,
// because the SET is the contract every consumer shares: the undefined-
// variable check, the argv expansion, and the fingerprint resolver must
// agree on exactly which names Pix owns, or one of them refuses an
// environment another one accepted. An empty home yields no variables
// rather than a variable defined as "": a home Pix could not resolve is
// not a fact it may state.
func PixManagedVars(pixHome string) map[string]string {
	home := strings.TrimSpace(pixHome)
	if home == "" {
		return map[string]string{}
	}
	return map[string]string{PixHomeVar: home}
}

// LookupPixManaged layers Pix's own variables OVER a host-environment
// probe, so `${PIX_HOME}` resolves to the home Pix actually uses even on a
// host that exported a different one to some other process, and every
// other name still falls through to base unchanged. A nil base means
// "nothing else is defined".
func LookupPixManaged(vars map[string]string, base func(string) (string, bool)) func(string) (string, bool) {
	return func(name string) (string, bool) {
		if v, ok := vars[name]; ok {
			return v, true
		}
		if base == nil {
			return "", false
		}
		return base(name)
	}
}

// ExpandPixManaged replaces every `${VAR}` / `${VAR:-default}` reference
// to a PIX-MANAGED variable in value, and leaves every other reference
// exactly as authored for sbx to resolve. Resolving only Pix's own names
// here is deliberate: a host `${VAR}` may carry a credential-shaped value,
// and the effective document is persisted under `.state/effective/`, so
// Pix expands only values it authored itself and never widens that file
// into a resolved-secret sink.
func ExpandPixManaged(value string, vars map[string]string) string {
	if value == "" || len(vars) == 0 {
		return value
	}
	return interpolationRE.ReplaceAllStringFunc(value, func(m string) string {
		g := interpolationRE.FindStringSubmatch(m)
		if g == nil {
			return m
		}
		v, ok := vars[g[1]]
		if !ok {
			return m
		}
		return v
	})
}

// ExpandPixManagedArgv expands Pix-managed variables through one whole
// argv, returning a new slice. It is the form both composition sites need
// (a command plus its ordered args), so neither has to write the loop and
// they cannot drift apart in which elements they expand.
func ExpandPixManagedArgv(argv []string, vars map[string]string) []string {
	if len(argv) == 0 || len(vars) == 0 {
		return argv
	}
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = ExpandPixManaged(a, vars)
	}
	return out
}

// interpolationRE matches one `${VAR}` or `${VAR:-default}` host-variable
// reference. It does not attempt to resolve either capture group against
// anything — see doc.go's "Interpolation is surfaced, never resolved".
var interpolationRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// scanInterpolations returns one Interpolation per `${VAR}`/`${VAR:-def}`
// reference found in value, attributed to keyPath. It never inspects the
// host environment: it is a pure string scan.
func scanInterpolations(value, keyPath string) []Interpolation {
	if value == "" {
		return nil
	}
	matches := interpolationRE.FindAllStringSubmatch(value, -1)
	if len(matches) == 0 {
		return nil
	}
	out := make([]Interpolation, 0, len(matches))
	for _, m := range matches {
		item := Interpolation{Var: m[1], KeyPath: keyPath}
		if m[2] != "" {
			def := m[3]
			item.Default = &def
		}
		out = append(out, item)
	}
	return out
}
