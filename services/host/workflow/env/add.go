// add.go — `pix env add <git-url|local-directory> [name]`: the one narrow
// exception to "Pix does not provide general environment create, edit,
// add, forget, update, or delete commands" (docs/design/pix-v2-surface.md
// §3.4). It ADOPTS a source that already exists — a local directory
// already on disk, or a git URL — as a new named environment under
// envs/<name>: a local directory is linked in with a symlink to its
// canonical absolute path, a git URL is cloned. There is still no
// registration database, no edit, no forget, and — unlike v1's
// `pix env add`, which wrote an `[environments]` entry into config.toml —
// this Add never touches config.toml: the directory IT CREATES under
// envs/ is the whole registration, exactly like one a user made by hand.
//
// Add is add-only. It never overwrites, merges, or replaces an existing
// envs/<name> entry of ANY kind (directory, symlink, or file); a caller
// that wants a different source under that name must first remove the
// existing entry with ordinary filesystem tools. It never selects a
// default, trusts, runs setup, or launches anything — those remain
// separate, explicit commands a human still has to run. It needs no TTY:
// naming a source (and optionally a name) is itself the whole approval
// this command asks for, unlike `pix env trust`'s host-execution consent.
package env

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"pix/host/cli"
	"pix/host/pixhome"
)

// AddResult is what Add actually did: which name it claimed, which kind of
// source it adopted ("local" or "git"), and the resolved root a later
// `pix env show <name>` will read from.
type AddResult struct {
	Name string
	Kind string
	Root string
}

// GitClone runs a real `git clone` as a subprocess. It is a package var,
// not a hardwired exec.Command call inside Add, so a test can substitute a
// deterministic double instead of a real git process ever touching the
// network — the same seam pixhome.Runner and workflow/task's own run()
// give their own host command calls.
var GitClone = func(url, dest string) (string, error) {
	cmd := exec.Command("git", "clone", "--quiet", "--", url, dest)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// scpLikeGitRE recognizes the scp-style remote spelling `user@host:path`
// (no "://"), the other shape `git clone` itself accepts alongside an
// ordinary scheme URL.
var scpLikeGitRE = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:.+$`)

// looksLikeGitURL reports whether raw is shaped like something `git clone`
// would accept as a remote: an ordinary "scheme://" URL (https, ssh, git,
// file, ...) or the scp-style "user@host:path" spelling. It is a SHAPE
// check only — Add never dials out to confirm a URL resolves before
// attempting the clone, exactly like `git clone` itself does not either.
func looksLikeGitURL(raw string) bool {
	if strings.Contains(raw, "://") {
		return true
	}
	return scpLikeGitRE.MatchString(raw)
}

// invalidNameRunRE collapses any run of characters ValidName rejects into a
// single "-" — the same character class ResolveIn's own nameRE allows.
var invalidNameRunRE = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizeCandidateName turns an arbitrary directory basename or URL path
// segment into a name ValidName accepts, or "" when nothing safe survives
// (a caller must then supply an explicit name).
func sanitizeCandidateName(raw string) string {
	s := invalidNameRunRE.ReplaceAllString(strings.TrimSpace(raw), "-")
	for len(s) > 0 && !isAlnumByte(s[0]) {
		s = s[1:]
	}
	if len(s) > 64 {
		s = s[:64]
	}
	s = strings.TrimRight(s, "-._")
	if !ValidName(s) {
		return ""
	}
	return s
}

func isAlnumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// deriveNameFromGitURL derives a candidate name from a git URL's final path
// segment (the repository name), stripping a query/fragment and a trailing
// ".git" the way every git host spells its own clone URL.
func deriveNameFromGitURL(raw string) string {
	s := raw
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimRight(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	return sanitizeCandidateName(s)
}

// deriveNameFromLocalPath derives a candidate name from a canonical local
// directory's basename.
func deriveNameFromLocalPath(canonical string) string {
	return sanitizeCandidateName(filepath.Base(canonical))
}

// classifySource decides whether source names an existing local directory
// or a git URL to clone, resolving a local candidate to its canonical
// absolute path (filepath.Abs, then EvalSymlinks) so the symlink Add later
// creates points directly at the real target, never through an extra
// symlink hop of its own — ResolveIn only ever follows one. A source that
// is neither an existing directory nor shaped like a git URL is refused
// before anything is created.
func classifySource(source string) (kind, canonicalLocal string, err error) {
	abs, absErr := filepath.Abs(source)
	if absErr == nil {
		if fi, statErr := os.Stat(abs); statErr == nil {
			if !fi.IsDir() {
				return "", "", cli.Usagef("pix: %s exists but is not a directory; refusing to adopt it", source)
			}
			canonical := abs
			if resolved, evalErr := filepath.EvalSymlinks(abs); evalErr == nil {
				canonical = resolved
			}
			return "local", canonical, nil
		}
	}
	if looksLikeGitURL(source) {
		return "git", "", nil
	}
	return "", "", cli.Usagef("pix: %q is neither an existing local directory nor a recognized git URL (scheme://... or user@host:path)", source)
}

// AddOptions is Add's input.
type AddOptions struct {
	Home   pixhome.Paths
	Source string // a git URL, or a path to an existing local directory
	Name   string // "" derives a name from Source
}

// Add adopts Source as a new environment named Name (or a name derived
// from Source when Name is ""). See the package-level doc comment above
// for the full contract. After creating the symlink or the clone, Add
// loads the result through the EXACT same ResolveIn + LoadHome path a real
// launch uses; a missing or invalid .sbxenv.yaml — or any other LoadHome
// refusal (an unsafe mode, a containment violation, a reserved MCP name,
// ...) — rolls back ONLY the artifact THIS call just created and returns
// that error. An existing target that Add did not itself just create is
// NEVER removed: rollback is scoped to this invocation's own side effect,
// nothing else.
func Add(o AddOptions) (AddResult, error) {
	home := o.Home
	source := strings.TrimSpace(o.Source)
	if source == "" {
		return AddResult{}, cli.Usagef("pix: env add: a git URL or local directory is required")
	}

	kind, canonicalLocal, err := classifySource(source)
	if err != nil {
		return AddResult{}, err
	}

	name := strings.TrimSpace(o.Name)
	if name != "" {
		if !ValidName(name) {
			return AddResult{}, cli.Usagef("pix: %q is not a valid environment name (letters, digits, dot, dash, underscore)", name)
		}
	} else {
		if kind == "local" {
			name = deriveNameFromLocalPath(canonicalLocal)
		} else {
			name = deriveNameFromGitURL(source)
		}
		if name == "" {
			return AddResult{}, cli.Usagef("pix: could not derive a safe environment name from %q; pass one explicitly: pix env add %s <name>", source, source)
		}
	}

	target := home.EnvironmentDir(name)
	if _, statErr := os.Lstat(target); statErr == nil {
		return AddResult{}, cli.Usagef("pix: environment %q already exists at %s; refusing to overwrite, merge, or replace it", name, target)
	} else if !os.IsNotExist(statErr) {
		return AddResult{}, statErr
	}

	if kind == "local" && (canonicalLocal == filepath.Clean(home.Home) || withinDir(canonicalLocal, filepath.Clean(home.Home))) {
		return AddResult{}, cli.Usagef("pix: refusing to link %s: it is PIX_HOME itself or a directory inside it", canonicalLocal)
	}

	if err := os.MkdirAll(home.Envs, 0o700); err != nil {
		return AddResult{}, fmt.Errorf("pix: create %s: %w", home.Envs, err)
	}

	if kind == "local" {
		if err := os.Symlink(canonicalLocal, target); err != nil {
			return AddResult{}, fmt.Errorf("pix: link %s -> %s: %w", target, canonicalLocal, err)
		}
	} else {
		out, cloneErr := GitClone(source, target)
		if cloneErr != nil {
			_ = os.RemoveAll(target)
			return AddResult{}, fmt.Errorf("pix: git clone %s: %w\n%s", source, cloneErr, strings.TrimSpace(out))
		}
	}

	sel, err := ResolveIn(home, name)
	if err == nil {
		_, err = LoadHome(sel, nil, nil)
	}
	if err != nil {
		removeAddedArtifact(kind, target)
		return AddResult{}, fmt.Errorf("pix: %s adopted at %s, but it did not pass validation; removed it.\n     cause: %w", source, target, err)
	}

	return AddResult{Name: name, Kind: kind, Root: sel.Root}, nil
}

// removeAddedArtifact rolls back exactly the artifact Add just created: for
// "local" that is the symlink ITSELF, never the source directory it points
// to; for "git" that is the freshly cloned directory tree.
func removeAddedArtifact(kind, target string) {
	if kind == "local" {
		_ = os.Remove(target)
	} else {
		_ = os.RemoveAll(target)
	}
}
