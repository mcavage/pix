package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// environment.go: the schema-level half of Story 1 (native sandbox
// environments, docs/design/environments.md §5.3). It owns exactly the two
// invariants the design calls out for `config.toml`:
//
//   - registration helpers accept a leading `~`, but only a CANONICAL
//     ABSOLUTE path is ever assigned to Environments;
//   - `environment` and `environments.*` have no hand-edit path — every
//     writer here is a method a `pix env` verb (Wave C) calls, never
//     something `pix config set/unset` reaches (see
//     workflow/provision/config.go's environmentKeyRefusal).
//
// It does not implement the `pix env` verbs, host trust review, or launch
// wiring; those are later Wave C/Story 1 units.

// CanonicalEnvironmentPath expands a leading `~` (via $HOME), then resolves
// the result to an absolute, cleaned path. It does not require the path to
// exist — registration may name a directory `pix env add` is about to
// scaffold. This is the ONLY transform AddEnvironment applies before storing,
// so it is exported for a caller (a future `pix env` verb, or a test) that
// needs the exact canonical form without going through the registry.
func CanonicalEnvironmentPath(path string) (string, error) {
	expanded, err := expandHome(path)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(expanded)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// expandHome expands a leading "~" or "~/..." to the user's home directory.
// Anything else (including "~otheruser/...", which this does not special-case)
// passes through unchanged.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve ~: %w", err)
	}
	if path == "~" {
		return home, nil
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

// IsCanonicalEnvironmentPath reports whether path is already exactly what
// CanonicalEnvironmentPath would produce: absolute, no leading `~`, and equal
// to its own filepath.Clean. config.Load's own dropNoncanonicalEnvironments
// uses this to fail closed on a hand-edited value; it is exported so a
// caller one layer up (workflow/env's ResolveEnvironment) can apply the SAME
// check to a registered root immediately after resolution — before its own
// first filepath.Join/os.Stat/os.Lstat — rather than trusting that whatever
// config.Load already sanitized on disk is the only way a *Config ever
// reaches it (a caller can also build one in memory, bypassing that pass
// entirely).
func IsCanonicalEnvironmentPath(path string) bool {
	if path == "" || strings.HasPrefix(path, "~") || !filepath.IsAbs(path) {
		return false
	}
	return filepath.Clean(path) == path
}

// environmentNameRE is the documented safe shape for an environment name: it
// must start with a letter or digit, then hold only letters, digits, '.',
// '_', '-', capped at 128 bytes. It is byte-for-byte the same pattern
// recreatelog's envNameRE enforces on the diagnostic log it keys by this same
// name (recreatelog/recreatelog.go, docs/design/environments.md section
// 10.2) — duplicated here rather than imported, because config is L0
// (foundation) and recreatelog is L1 (capability): an L0 package may never
// import a capability (see ../arch_test.go). recreatelog's own
// TestEnvironmentNameShapeMatchesConfig is the parity check that keeps the
// two definitions from drifting apart; it lives there (not here) because L1
// may legally import L0, not the reverse.
var environmentNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// validEnvironmentName reports whether name is safe to both write into
// `config.toml`'s `[environments]` table AND thread through to a filesystem-
// adjacent identifier (recreatelog's environment field, a future `pix env`
// verb's naming). It rejects a leading space, a slash, a control character,
// leading punctuation (anything but a letter or digit to start), and
// anything over 128 bytes.
func validEnvironmentName(name string) bool {
	return environmentNameRE.MatchString(name)
}

// AddEnvironment registers name against path in the Environments index,
// canonicalizing path first (see CanonicalEnvironmentPath) so what gets
// persisted is always absolute regardless of what the caller typed. It
// overwrites an existing registration under the same name. Returns the
// canonical path actually stored.
//
// An empty or whitespace-only path is refused outright rather than passed to
// CanonicalEnvironmentPath: filepath.Abs("") resolves to the current working
// directory, so a blank path would silently register whatever directory the
// caller happened to be standing in as the environment root instead of
// failing loudly. Same for name: a whitespace-only name would otherwise
// register under a name indistinguishable from empty once trimmed elsewhere.
func (c *Config) AddEnvironment(name, path string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("environment name must not be empty")
	}
	if !validEnvironmentName(name) {
		return "", fmt.Errorf("environment name %q must start with a letter or digit and contain only letters, digits, '.', '_', '-' (max 128 bytes)", name)
	}
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("environment path must not be empty")
	}
	canon, err := CanonicalEnvironmentPath(path)
	if err != nil {
		return "", err
	}
	if c.Environments == nil {
		c.Environments = map[string]string{}
	}
	c.Environments[name] = canon
	return canon, nil
}

// RemoveEnvironment unregisters name, returning true when it changed. If name
// was the machine default (Environment), the default is cleared too — a
// default may never dangle, naming a registration that no longer exists.
func (c *Config) RemoveEnvironment(name string) bool {
	if _, ok := c.Environments[name]; !ok {
		return false
	}
	delete(c.Environments, name)
	if c.Environment == name {
		c.Environment = ""
	}
	return true
}

// UnknownEnvironmentError is returned by UseEnvironment when name is not in
// Environments. It carries the structured pieces (the rejected Name, and
// Known — the registry's names, sorted for a deterministic message) rather
// than only a formatted string, so a caller that needs a DIFFERENT
// presentation (JSON, a non-TTY short form) can still build one from Name/
// Known without re-parsing Error()'s prose. Error() itself is
// docs/design/environments.md §8.1's actionable shape verbatim (the PRD's
// §5.1 counterpart): the failure statement, `known:`, an OPTIONAL
// `closest:` (see below), and the register-one fix.
//
// `closest:` is §8.1's structured suggestion (D14): a single labelled fact,
// printed only when exactly one registered name is unambiguously close to
// Name, computed by closestKnownName below — never a question, never an
// offer to act on Name's behalf.
//
// `pix env add` is the ONLY command this message references, and it is named
// here purely as the fix a user would type; the register-one line is fixed
// literal text (`pix env add <name> [path]`), never Name itself — echoing
// the typo back as "the fix" would read as an instruction to register the
// typo unchanged.
type UnknownEnvironmentError struct {
	Name  string
	Known []string // sorted; empty (not nil) when the registry is empty
}

// closestKnownNameThreshold returns the maximum Levenshtein edit distance
// closestKnownName still treats as "close enough to suggest", scaled by how
// short name is: a short name has less room before a suggestion stops being
// trustworthy (a distance of 2 on a 4-character name is HALF the string), so
// a name of 4 runes or fewer gets the tighter threshold. The two constants
// are docs/design/environments.md §8.1's own tuning, not derived from
// anything else.
func closestKnownNameThreshold(name string) int {
	if len([]rune(name)) <= 4 {
		return 1
	}
	return 2
}

// closestKnownName implements `closest:` (D14): case-sensitive Levenshtein
// edit distance (insert/delete/substitute, each cost 1 — there is no
// separate, cheaper transposition move, so swapping two adjacent characters
// costs 2, not 1) between name and each entry of known, filtered to
// closestKnownNameThreshold(name) or less. A match is offered only when
// exactly one known name attains the minimum distance found: two or more
// names tied at that minimum, every known name farther than the threshold,
// or an empty known list, all return ok=false — "which one?" is not
// answerable from a tie, so none is offered rather than guessing.
//
// The no-transposition-discount choice is deliberate and has a visible
// consequence worth naming: "hoem" is Levenshtein-distance 2 from "home"
// (both characters at the swapped positions are substitutions), which is
// farther than a 4-rune name's threshold of 1 tolerates — so "home" is NOT
// offered as `hoem`'s closest match even though a human reads the typo as
// obvious. That is this algorithm's documented behavior, not a bug: a
// looser (Damerau-Levenshtein) distance would need its own threshold
// re-tuning, which is a decision for whoever needs transposition typos
// caught, not an accidental side effect of this one.
func closestKnownName(name string, known []string) (string, bool) {
	threshold := closestKnownNameThreshold(name)
	best := ""
	bestDistance := threshold + 1
	tie := false
	for _, k := range known {
		d := levenshteinDistance(name, k)
		if d > threshold {
			continue
		}
		switch {
		case d < bestDistance:
			best, bestDistance, tie = k, d, false
		case d == bestDistance:
			tie = true
		}
	}
	if best == "" || tie {
		return "", false
	}
	return best, true
}

// levenshteinDistance is the classic case-sensitive edit distance (insert,
// delete, substitute — no transposition move; see closestKnownName's own
// doc comment for why that matters here), computed over runes so a
// multi-byte name never miscounts. It is a small, independent copy of the
// same algorithm cmd/pix's suggestVerb already runs for a mistyped VERB
// (help.go's own levenshtein): config cannot import cmd/pix (the dependency
// runs the other way), and the two operate over unrelated vocabularies
// (verbs vs. registered environment names), so sharing one helper package
// for a ~15-line function would be premature coupling, not
// deduplication.
func levenshteinDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	cur := make([]int, len(rb)+1)
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// Error renders docs/design/environments.md §8.1's actionable copy: the
// failure statement, `known:`, an optional `closest:` (closestKnownName;
// omitted whenever it returns ok=false), and the fixed register-one line.
// The register-one line is fixed literal text (`pix env add <name>
// [path]`) — it never interpolates the mistyped Name.
func (e *UnknownEnvironmentError) Error() string {
	known := "none"
	if len(e.Known) > 0 {
		known = strings.Join(e.Known, ", ")
	}
	msg := fmt.Sprintf("pix: no environment named %q.\n     known: %s", e.Name, known)
	if closest, ok := closestKnownName(e.Name, e.Known); ok {
		msg += fmt.Sprintf("\n     closest: %s", closest)
	}
	return msg + "\n     register one: pix env add <name> [path]"
}

// UseEnvironment sets the machine default environment NAME. name must already
// be registered in Environments, or empty to clear the default outright. This
// enforces only "this name exists" — the host trust review `pix env use`
// (workflow/env.Use) performs before selecting a Tier1 environment is a
// separate, layered-on-top gate, not something this schema-level method
// knows about.
//
// On refusal it returns an *UnknownEnvironmentError and leaves Environment
// (and Environments) untouched: a rejected selection is never a partial
// mutation.
func (c *Config) UseEnvironment(name string) error {
	if name == "" {
		c.Environment = ""
		return nil
	}
	if _, ok := c.Environments[name]; !ok {
		known := make([]string, 0, len(c.Environments))
		for n := range c.Environments {
			known = append(known, n)
		}
		slices.Sort(known)
		return &UnknownEnvironmentError{Name: name, Known: known}
	}
	c.Environment = name
	return nil
}
