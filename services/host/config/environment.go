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
//   - `environment` and `environments.*` have no hand-edit path — the
//     methods below were written for a v1 `pix env` verb design and have
//     no live caller in v2 (whose `pix env default NAME` writes the machine
//     default through pixhome.SetDefaultEnvironment instead), but no
//     generic config-mutation verb reaches these keys either (see
//     workflow/provision/config.go's environmentKeyRefusal).
//
// It does not implement the `pix env` verbs, host trust review, or launch
// wiring; those are later Wave C/Story 1 units.

// CanonicalEnvironmentPath expands a leading `~` (via $HOME), then resolves
// the result to an absolute, cleaned path. It does not require the path to
// exist — registration may name a directory that has not been scaffolded
// yet. This is the ONLY transform AddEnvironment applies before storing, so
// it is exported for a caller (a test, or any future registry-style writer)
// that needs the exact canonical form without going through the registry.
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
// This is the v1 registry's error shape; v2 has no registration verb of
// any kind to name as the fix — an environment is a plain
// directory under ~/.pix/envs a user creates by hand — so the register-one
// line below is fixed, generic prose, never Name itself: echoing the typo
// back as "the fix" would read as an instruction to register the typo
// unchanged.
type UnknownEnvironmentError struct {
	Name  string
	Known []string // sorted; empty (not nil) when the registry is empty
}

// closestKnownNameThreshold returns the maximum edit distance
// (optimalStringAlignmentDistance) closestKnownName still treats as "close enough to suggest", scaled by how
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

// closestKnownName implements `closest:` (D14): case-sensitive
// adjacent-transposition-aware edit distance (optimalStringAlignmentDistance
// below) between name and each entry of known, filtered to
// closestKnownNameThreshold(name) or less. A match is offered only when
// exactly one known name attains the minimum distance found: two or more
// names tied at that minimum, every known name farther than the threshold,
// or an empty known list, all return ok=false — "which one?" is not
// answerable from a tie, so none is offered rather than guessing.
//
// The transposition discount is deliberate and has a visible, PRD-pinned
// consequence: docs/design/environments.md §8.1's own worked example is
// `hoem` against a registry containing `home` (plus `work`, `luna`, both
// far away) — a plain Levenshtein distance charges two substitutions for
// that swap (distance 2), which a 4-rune name's threshold of 1 would
// reject; optimalStringAlignmentDistance's one adjacent-swap move prices
// the SAME pair at distance 1, so `home` is offered, uniquely, exactly as a
// human reading `hoem` as an obvious transposition would expect.
// TestUseEnvironmentUnknownErrorShape pins this end to end through
// Error()'s rendered `closest:` line.
func closestKnownName(name string, known []string) (string, bool) {
	threshold := closestKnownNameThreshold(name)
	best := ""
	bestDistance := threshold + 1
	tie := false
	for _, k := range known {
		d := optimalStringAlignmentDistance(name, k)
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

// optimalStringAlignmentDistance is a case-sensitive Damerau-Levenshtein
// variant known as "optimal string alignment" (OSA): the same
// insert/delete/substitute moves as plain Levenshtein, PLUS one adjacent
// transposition move (swapping two neighboring runes) at the SAME cost of
// 1 as a substitution — so "hoem"/"home" costs 1, not 2. OSA differs from
// full ("unrestricted") Damerau-Levenshtein in one deliberate way: it
// never edits a substring more than once (a transposed pair is never
// itself later substituted or transposed again), which makes it NOT a
// true metric (it can violate the triangle inequality on pathological
// inputs) but keeps the classic O(len(a)*len(b)) dynamic-programming table
// — no extra bookkeeping of "last time this rune pair appeared" the
// unrestricted algorithm needs. That tradeoff is irrelevant here:
// closestKnownName only ever compares name against ONE known entry at a
// time and takes the minimum, never chains distances, so the metric
// property was never load-bearing.
//
// Computed over runes so a multi-byte name never miscounts. It is a small,
// independent copy of the same edit-distance IDEA cmd/pix's suggestVerb
// already runs for a mistyped VERB (help.go's own levenshtein, plain
// Levenshtein with no transposition move): config cannot import cmd/pix
// (the dependency runs the other way), and the two operate over unrelated
// vocabularies (verbs vs. registered environment names) with different
// tuning needs, so sharing one helper package for a ~25-line function
// would be premature coupling, not deduplication.
func optimalStringAlignmentDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	la, lb := len(ra), len(rb)
	d := make([][]int, la+1)
	for i := range d {
		d[i] = make([]int, lb+1)
		d[i][0] = i
	}
	for j := 0; j <= lb; j++ {
		d[0][j] = j
	}
	for i := 1; i <= la; i++ {
		for j := 1; j <= lb; j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+cost)
			}
		}
	}
	return d[la][lb]
}

// Error renders docs/design/environments.md §8.1's actionable copy: the
// failure statement, `known:`, an optional `closest:` (closestKnownName;
// omitted whenever it returns ok=false), and a fixed register-one line. The
// register-one line is fixed generic prose, never interpolating the
// mistyped Name — v2 has no registration verb, so it points at creating a
// directory under ~/.pix/envs rather than naming a registration command
// this codebase no longer has.
func (e *UnknownEnvironmentError) Error() string {
	known := "none"
	if len(e.Known) > 0 {
		known = strings.Join(e.Known, ", ")
	}
	msg := fmt.Sprintf("pix: no environment named %q.\n     known: %s", e.Name, known)
	if closest, ok := closestKnownName(e.Name, e.Known); ok {
		msg += fmt.Sprintf("\n     closest: %s", closest)
	}
	return msg + "\n     create one: a directory under ~/.pix/envs/<name>"
}

// UseEnvironment sets the machine default environment NAME. name must already
// be registered in Environments, or empty to clear the default outright. This
// enforces only "this name exists" — the host trust review workflow/env.Use
// performs before selecting a Tier1 environment is a separate, layered-on-top
// gate, not something this schema-level method knows about. v2's actual
// default-setting verb is `pix env default NAME`, which writes through
// pixhome.SetDefaultEnvironment instead of this v1 registry method (nothing
// in the current CLI calls UseEnvironment any more).
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
