package env

import (
	"fmt"
	"os"
	"os/exec"
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

// NoncanonicalRootError is ResolveEnvironment's refusal when a registered
// environment's stored root is not already exactly what config's own
// canonicalization (config.IsCanonicalEnvironmentPath) would produce: an
// absolute, clean path with no leading `~`. config.AddEnvironment (E1.5)
// never persists anything else, and config.Load's own
// dropNoncanonicalEnvironments already drops a hand-edited entry that
// somehow reached config.toml — but a *config.Config a caller assembles
// directly (a test, a future in-memory composition) never runs that pass at
// all, so ResolveEnvironment applies the SAME check itself rather than
// trusting cfg.Environments unconditionally. This is what stops a relative
// or `~`-prefixed value from ever reaching this package's first
// filepath.Join/os.Stat/os.Lstat, which would otherwise resolve it against
// the CALLING PROCESS's own working directory — silently reading whatever
// happens to sit there instead of refusing outright.
type NoncanonicalRootError struct {
	Name string
	Root string
}

func (e *NoncanonicalRootError) Error() string {
	return fmt.Sprintf(
		"pix: environment %q's registered root is not a canonical absolute path; refusing rather than resolving it against the current directory.\n     root: %s\n     re-register it: pix env add %s <path>",
		e.Name, e.Root, e.Name,
	)
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
		"pix: environment root %s resolves inside writable workspace %s it mounts; refusing.\n     workspace: %s\n     register a root outside it: pix env add <name> <path>",
		e.Root, e.Workspace, e.Workspace,
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
// A lexically-canonical path can still be a SYMLINK ALIAS of somewhere
// else: root itself, an ancestor directory of root, the workspace itself,
// or an ancestor of the workspace, can each be a symlink whose target sits
// on the opposite side of the containment boundary from what the authored
// path string suggests (a workspace registered by its symlinked alias while
// the environment root sits under the physical target, or the reverse).
// RefuseSymlinkedRoot only Lstats root itself — it never walks root's
// ancestors — so it cannot catch an aliased ANCESTOR component; a workspace
// path is never symlink-checked at all before this function runs. So when a
// canonical path actually EXISTS on disk, physicalPath additionally resolves
// it with filepath.EvalSymlinks before the within check, and the within
// check compares those PHYSICAL forms rather than the lexical ones — that is
// the only comparison a symlink alias on either side cannot bypass. A path
// that does not exist yet (the common case in a pure location-arithmetic
// test, and legitimately possible for a workspace declaration this function
// itself does not create) has nothing to resolve, so it falls back to the
// lexical canonical form unchanged, exactly as before this check existed.
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
// the underlying *ContainmentError through UsageError's Unwrap. Error() is
// always constructed from the AUTHORED canonical forms (canonRoot/canonWS),
// never the resolved physical ones: a user registered and reads back the
// path they authored, not an internal symlink target they may never have
// seen.
func RefuseContainment(root string, workspaces []string) error {
	canonRoot := hosttrust.CanonicalRoot(root)
	physRoot, rootExisted, rootErr := physicalPath(canonRoot)
	if rootExisted && rootErr != nil {
		// Fail closed: root exists but its physical location could not be
		// established (a permission failure, a symlink loop, ...) — refuse
		// rather than silently falling back to a lexical comparison a real
		// alias could defeat. There is no workspace to name yet at this
		// point, so the authored root itself stands in for Workspace too;
		// this is the same fields-both-set shape every other ContainmentError
		// carries, just naming the side that actually failed.
		return cli.UsageError{Err: &ContainmentError{Root: canonRoot, Workspace: canonRoot}}
	}
	for _, ws := range workspaces {
		canonWS := hosttrust.CanonicalRoot(ws)
		if canonWS == "" {
			continue
		}
		physWS, wsExisted, wsErr := physicalPath(canonWS)
		if wsExisted && wsErr != nil {
			return cli.UsageError{Err: &ContainmentError{Root: canonRoot, Workspace: canonWS}}
		}
		if withinDir(physRoot, physWS) {
			return cli.UsageError{Err: &ContainmentError{Root: canonRoot, Workspace: canonWS}}
		}
	}
	return nil
}

// physicalPath resolves p's real, symlink-free location with
// filepath.EvalSymlinks when p exists on disk. existed reports whether p
// was found at all (an Lstat probe, no-follow, so a symlink whose target is
// missing still counts as "existed" — there IS something at p to resolve,
// and EvalSymlinks below is what actually reports whether resolving it
// failed). When p does not exist, physicalPath returns p unchanged and
// existed=false: nothing to resolve is not a failure, and RefuseContainment
// falls back to the lexical canonical form for exactly that case.
func physicalPath(p string) (resolved string, existed bool, err error) {
	if _, statErr := os.Lstat(p); statErr != nil {
		return p, false, nil
	}
	resolved, err = filepath.EvalSymlinks(p)
	if err != nil {
		return p, true, err
	}
	return resolved, true, nil
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

// ResolveLocalCommand resolves raw — a native `mcp.servers[].command` or a
// pix.toml `[[host.services]].command` reference — to the absolute
// filesystem path Load's symlink check must inspect, without ever touching
// the calling process's own working directory (the same "resolves
// identically from any cwd" invariant AC-10 already holds Resolve to):
//
//   - a BARE name (no path separator) is looked up on PATH via lookPath —
//     the exec.LookPath production seam a test overrides to pin a
//     symlinked, regular, or missing binary without touching the real PATH.
//     A nil lookPath defaults to the real exec.LookPath.
//   - a RELATIVE path containing a separator resolves against root, the
//     environment's own canonical root — never the caller's cwd.
//   - an ABSOLUTE path is returned unchanged (cleaned).
//
// The second return is false when raw could not be resolved to a concrete
// path at all (lookPath found nothing on PATH for a bare name): that is not
// itself a symlink refusal, only "nothing local to check" — the same
// silence the prior os.Lstat-based check gave a nonexistent file. A missing
// binary is a `pix doctor`-shaped gap (health.MCPProbe already reports it),
// not this package's concern.
func ResolveLocalCommand(root, raw string, lookPath func(string) (string, error)) (string, bool) {
	if raw == "" {
		return "", false
	}
	if filepath.IsAbs(raw) {
		return filepath.Clean(raw), true
	}
	if strings.ContainsRune(raw, filepath.Separator) || strings.ContainsRune(raw, '/') {
		return filepath.Join(root, raw), true
	}
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	resolved, err := lookPath(raw)
	if err != nil {
		return "", false
	}
	return resolved, true
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
