package env

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hosttrust"
)

// Resolve returns the canonical root registered under name in cfg. It is a
// pure exact-name lookup — no prefix or fuzzy fallback (docs/design/
// environments.md §8) — over cfg.Environments, which config.AddEnvironment
// only ever populates with an already-canonical absolute path. Resolve
// itself performs no path arithmetic of its own (no filepath.Abs, no
// consulting the working directory), so the same registered name returns
// the byte-identical root no matter what directory the calling process
// happens to be standing in when it asks (AC-10).
//
// An unregistered name returns *config.UnknownEnvironmentError — the same
// typed error config.UseEnvironment already returns — carrying the sorted
// list of every currently registered name, so a caller can render its own
// presentation (a "closest:" suggestion, JSON, a non-TTY short form)
// without re-parsing Error()'s prose.
func Resolve(cfg *config.Config, name string) (string, error) {
	root, ok := Root(cfg, name)
	if !ok {
		return "", &config.UnknownEnvironmentError{Name: name, Known: Known(cfg)}
	}
	return root, nil
}

// ContainmentError is RefuseContainment's structured refusal (AC-11): Root
// resolves inside Workspace, one of the writable workspaces the environment
// declares. Both fields are already-canonical absolute paths, so a caller
// printing Error() names both without any further resolution.
type ContainmentError struct {
	Root      string
	Workspace string
}

func (e *ContainmentError) Error() string {
	return fmt.Sprintf(
		"pix: environment root %s resolves inside writable workspace %s it mounts; refusing",
		e.Root, e.Workspace,
	)
}

// RefuseContainment refuses when root resolves inside ANY of workspaces —
// equal to it, or nested under it — per docs/design/environments.md §5.1
// restriction 4: "The canonical environment root must not resolve inside
// any writable workspace it mounts." root and every entry of workspaces are
// canonicalized (hosttrust.CanonicalRoot) before comparison, so the check is
// correct regardless of how either was authored (relative, `~`-prefixed,
// with a trailing slash, ...).
//
// workspaces is supplied by the caller, not derived here: this package
// takes no dependency on which sbx facet ultimately declares a writable
// workspace (mounts, the primary workspace, an additional one) — that
// composition belongs to whichever later unit builds the effective
// declaration (E1.8's bill of materials, E2.1's renderer). Injecting the
// list keeps this function pure location arithmetic, testable with >= 3 cwd
// values and no environment payload at all.
//
// The returned error is wrapped in cli.UsageError so cli.ExitCode(err) is 2
// (a usage/refusal, not an operational failure) while errors.As still finds
// the underlying *ContainmentError through UsageError's Unwrap.
func RefuseContainment(root string, workspaces []string) error {
	canonRoot := hosttrust.CanonicalRoot(root)
	for _, ws := range workspaces {
		canonWS := hosttrust.CanonicalRoot(ws)
		if canonWS == "" {
			continue
		}
		if withinDir(canonRoot, canonWS) {
			return cli.UsageError{Err: &ContainmentError{Root: canonRoot, Workspace: canonWS}}
		}
	}
	return nil
}

// withinDir reports whether path is dir itself, or nested under it. Both
// arguments must already be canonical (Abs + Clean); withinDir performs no
// canonicalization of its own.
func withinDir(path, dir string) bool {
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// SymlinkError is RefuseSymlinkedRoot/RefuseSymlinkedReference's structured
// refusal (AC-12). Kind names what Path is ("environment root", "local MCP
// command", "kit path", ...) so the rendered text is self-describing without
// a caller having to remember which refusal function produced it.
type SymlinkError struct {
	Kind string
	Path string
}

func (e *SymlinkError) Error() string {
	return fmt.Sprintf("pix: %s %s is a symlink; refusing", e.Kind, e.Path)
}

// RefuseSymlinkedRoot refuses when root itself (not a directory it happens
// to contain) is a symlink rather than a real directory — docs/design/
// environments.md §5.1's registered-root symlink restriction, half of
// AC-12. It uses hosttrust.IsSymlink (Lstat, no follow), the same primitive
// every other host-exec symlink refusal in this module uses.
func RefuseSymlinkedRoot(root string) error {
	if hosttrust.IsSymlink(root) {
		return cli.UsageError{Err: &SymlinkError{Kind: "environment root", Path: root}}
	}
	return nil
}

// RefuseSymlinkedReference refuses when a referenced host-execution surface
// — a local MCP command, a local kit path, or any other referenced
// executable a caller names via kind — is itself a symlink. It is the other
// half of AC-12: "a symlinked environment root or referenced executable [is]
// refused." kind becomes the SymlinkError's Kind, so the refusal text says
// exactly what was checked ("local MCP command warehouse-proxy", "kit path
// kits[0]", ...).
//
// It is the caller's job (E1.8's bill of materials, built from envinfo's
// pre-composition Tree) to decide WHICH tree nodes are referenced local
// paths worth this check in the first place; see RequiresSymlinkCheck for
// the local-vs-remote classification that decision should fail closed on.
func RefuseSymlinkedReference(kind, path string) error {
	if hosttrust.IsSymlink(path) {
		return cli.UsageError{Err: &SymlinkError{Kind: kind, Path: path}}
	}
	return nil
}

// remoteSchemeRE recognizes an unambiguous remote reference the same way
// envinfo's own kit classifier does: a leading URL-shaped scheme
// ("https://", "git+ssh://", ...) followed by "://".
var remoteSchemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)

// RequiresSymlinkCheck reports whether raw names a reference this package
// must symlink-check (RefuseSymlinkedReference) before a caller trusts it:
// true for everything except an unambiguous remote URL-shaped reference.
// "unknown local-vs-remote fails closed" is this function's whole contract:
// it never returns false because a shape could not be positively classified
// — only because it was POSITIVELY identified as remote. A caller handed an
// ambiguous or unrecognized shape (not a URL, not obviously a filesystem
// path either) still gets true here, and therefore still runs the strictest
// check rather than silently skipping it.
func RequiresSymlinkCheck(raw string) bool {
	return !remoteSchemeRE.MatchString(raw)
}

// SubjectKind is the hosttrust.Subject.Kind namespace under which every
// environment's host-exec acceptance record is filed. It is a KIND, not a
// NAME: an acceptance record is keyed by Subject{Kind: SubjectKind, Root:
// <canonical root>} (see Subject below) — a registered NAME never appears
// in the key at all.
const SubjectKind = "environment"

// Subject returns the hosttrust.Subject an environment's acceptance record
// is keyed by: this package's fixed SubjectKind plus root's canonical form
// (hosttrust.CanonicalRoot). Two different roots are always two different
// subjects.
func Subject(root string) hosttrust.Subject {
	return hosttrust.Subject{Kind: SubjectKind, Root: hosttrust.CanonicalRoot(root)}
}

// IsAccepted reports whether store already holds an accepted record for
// root's canonical Subject. It is pure lookup — no fingerprint comparison,
// no side effect — leaving "accepted AS OF WHICH fingerprint" to E1.8's
// review flow; this only answers "is there a record here at all".
//
// This is what makes AC-16 true: repointing a registered NAME to a new path
// (Register overwriting an existing config entry) changes what
// Resolve(name) returns, but it can never transfer acceptance, because
// acceptance was never keyed by name in the first place. The OLD root's
// Subject — and whatever record it holds — is untouched by a repoint; the
// NEW root is simply a Subject IsAccepted has never seen, and reports
// unaccepted accordingly.
func IsAccepted(store *hosttrust.AcceptanceStore, root string) bool {
	_, ok := store.Get(Subject(root))
	return ok
}
