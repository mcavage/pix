// pack.go implements `pi-stack pack` — the git-backed context bundle (skills +
// knowledge + later mcp/proxies/routing/config). See docs/design/packs.md.
//
// v1 (this file): the local, Tier-0 slice — pack.toml manifest, new (adopt an
// existing repo or git-init a fresh one), add skill|knowledge, ls, show, use, rm.
// No host execution, no git-URL adoption yet, no profile deletion; those are
// later increments. All OS/git calls go through defaultShellEnv so the logic is
// testable with fakes.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"pi-stack/host/config"
)

// packManifest is pack.toml. Identity + model prefs (v1), plus the v2 facets:
// F1 integrations attach (unchanged shape, now enabling), F2/F3 proxy wrappers,
// the (struct-only, P2) external-binary facet, F4 config layering (gog_account/
// memory_scope/routing), and F6 knowledge references. Skills and embedded
// knowledge are still discovered by convention (skills/, knowledge/), so a pack
// does not have to enumerate them.
type packManifest struct {
	Name              string `toml:"name"`
	Schema            int    `toml:"schema"`
	OllamaBridgeModel string `toml:"ollama_bridge_model,omitempty"`
	// GogAccount, when set, is layered into cfg.GogAccount on `pack use` (F4).
	GogAccount string `toml:"gog_account,omitempty"`
	// MemoryScope tags in-VM memory recall/capture (F4); default = the pack Name.
	// "default" (or the pack's own default) selects the shared/unscoped tag.
	MemoryScope string `toml:"memory_scope,omitempty"`
	// Routing is a STRUCT-ONLY placeholder for a pack-level routing override
	// (packs-v2-impl.md §2: "optional/stretch"). Nothing reads it in Phase 1.
	Routing      *packRouting      `toml:"routing,omitempty"`
	Integrations []packIntegration `toml:"integrations,omitempty"`
	// Proxies are [[proxy]] entries: F2 in-sandbox bin/ wrappers (Host unset/
	// false) and F3 host-mode wrappers (Host true, struct carried but NOT
	// installed/PATH-wired until Phase 2 — no host exec in this build).
	Proxies []packProxy `toml:"proxy,omitempty"`
	// Bins are [[bin]] external host binaries (Tier-1, SHA-pinned). STRUCT ONLY
	// in Phase 1: loadPack validates the shape (fail-closed on a missing sha) but
	// nothing executes one — that is the P2 trust-gated host-exec path.
	Bins []packBin `toml:"bin,omitempty"`
	// Knowledge are [[knowledge]] references (F6): shared=true travels (a git
	// URL an adopter pulls), shared=false does not (a local path, standalone).
	Knowledge []packKnowledge `toml:"knowledge,omitempty"`
}

// packRouting is the struct-only pack-level routing override placeholder
// (packs-v2-impl.md §2). Repo-relative paths; nothing wires them in Phase 1.
type packRouting struct {
	Policy    string `toml:"policy,omitempty"`
	Scorecard string `toml:"scorecard,omitempty"`
}

// packProxy is one [[proxy]] entry: a bin/<name> wrapper script. Host=false
// (default) is an F2 in-sandbox wrapper, synthesized into an ephemeral mixin
// kit at launch (synthesizePackKit). Host=true is an F3 host-mode wrapper —
// carried by the schema in Phase 1, but installation/PATH-wiring is P2.
type packProxy struct {
	Name   string   `toml:"name"`
	Host   bool     `toml:"host,omitempty"`
	Egress []string `toml:"egress,omitempty"`
}

// packBin is one [[bin]] entry: an external, SHA-pinned host binary (Tier-1,
// rare, P2). loadPack fails closed on an empty SHA (never reaches an exec path
// unpinned) even though Phase 1 never executes one.
type packBin struct {
	Name string `toml:"name"`
	Path string `toml:"path"`
	SHA  string `toml:"sha"`
	Host bool   `toml:"host,omitempty"`
}

// packKnowledge is one [[knowledge]] entry (F6): a reference to a bundle beyond
// the pack's own embedded knowledge/ dir. Shared=true travels with the pack (a
// git URL adopters pull); Shared=false does not (a local path, standalone —
// deliberately NOT repo-root-scoped, since pointing outside the pack is the
// entire point of a private reference).
type packKnowledge struct {
	Name   string `toml:"name"`
	Source string `toml:"source"`
	Shared bool   `toml:"shared,omitempty"`
}

// packIntegration is a REFERENCE-ONLY integration (v1): the pack says "I use
// <mcp> and need the credential <env>". It ships NO executable code — the MCP
// server is host-provided (gog, a gateway-catalog server), and the credential is
// solicited as an op:// ref the user owns. Pack-SHIPPED executables are v2.
type packIntegration struct {
	Name string `toml:"name"`          // human label
	Env  string `toml:"env,omitempty"` // op-refs.env ENV VAR the credential lives under
	MCP  string `toml:"mcp,omitempty"` // MCP server name to attach (host-provided)
	// Manifest, when set, makes this a CONTAINER integration: an OCI-packaged
	// stdio MCP server the sbx gateway runs on the host via Docker. The value is a
	// server-manifest URL (server.json/server.yaml, e.g. a GitHub raw or internal
	// HTTP URL) or a registry ref that `sbx mcp add --local --url <manifest>`
	// accepts. Registration uses that form (NOT the host `--command` path), so no
	// pi-stack-host recompile or private build is needed; the container's
	// credentials are provided Docker-side (declared in its server.json), not via
	// the op-run wrapper. Leave empty for a host-provided server (slack, gog) or a
	// remote gateway-catalog server.
	Manifest string `toml:"manifest,omitempty"`
	// Image, when set, is a CONTAINER integration registered by DIRECT image ref:
	// pi-stack registers it as `docker run -i --rm -e <KEY>… <image>`, op-run
	// wrapped exactly like slack (so its creds resolve from 1Password at gateway
	// spawn and forward into the container via `-e`). Simpler than Manifest — no
	// server.json to host: a locally-built image tag works, and a registry only
	// matters when you share it. Mutually exclusive with Manifest.
	Image string `toml:"image,omitempty"`
	// EnvKeys are ADDITIONAL (typically non-secret) env var names forwarded into an
	// Image container via `-e <KEY>` (e.g. BAMBOOHR_COMPANY_DOMAIN). The primary
	// op-refs-backed secret goes in Env (also forwarded, and warned about if unset).
	EnvKeys []string `toml:"env_keys,omitempty"`
	// URL, when set, makes this a REMOTE integration the pack registers ITSELF:
	// `pack use` runs `sbx mcp add <mcp> --url <url>` so the pack's remote
	// gateway-catalog servers (opine, notion, atlassian, granola) are wired without
	// a manual `pi-stack mcp bundle` + `sbx mcp add`. The URL is a remote MCP
	// endpoint (https://host/mcp); OAuth is discovered + handled host-side by the
	// gateway on first use (no credential in the pack). Mutually exclusive with
	// Manifest/Image. Leave empty for a server the user registers out-of-band.
	URL string `toml:"url,omitempty"`
}

// packContainer is a resolved pack CONTAINER/REMOTE integration: a Manifest
// (`sbx mcp add --local --url`, gateway resolves the OCI image; creds Docker-side),
// an Image ref (`docker run <image>`, op-run wrapped; creds from op-refs forwarded
// via EnvKeys), or a RemoteURL (`sbx mcp add --url`, a remote MCP endpoint the
// gateway OAuths host-side). Exactly one of Manifest/Image/RemoteURL is set.
type packContainer struct {
	Manifest  string
	Image     string
	EnvKeys   []string // env var names to forward into an Image container (-e KEY)
	RemoteURL string   // remote MCP endpoint URL (`sbx mcp add <name> --url <url>`)
}

// packInfo is a resolved pack on disk.
type packInfo struct {
	Root         string
	Manifest     packManifest
	SkillsDir    string // <root>/skills if it exists, else ""
	KnowledgeDir string // <root>/knowledge if it exists, else ""
	BinDir       string // <root>/bin if it exists, else "" (F2/F3 proxy wrapper scripts)
	// CapabilitiesFile is <root>/capabilities.json if it exists (a regular file),
	// else "". Mounted into the sandbox at ~/.pi/agent/capabilities.json via the
	// synthesized mixin kit so a pack carries its own capability->provider routing
	// (what used to require the private overlay kit).
	CapabilitiesFile string
}

const packManifestName = "pack.toml"

// errNotAPack is the sentinel loadPack wraps when root is not a pack AT ALL
// (no pack.toml, including the root directory itself being gone). Callers use
// errors.Is to distinguish this "genuinely absent" class — safe to degrade on
// (e.g. a stale cfg.Pack pointing at a deleted dir) — from every OTHER load
// error (symlink rejection, facet validation, parse failure), which means a
// pack that EXISTS but is broken or tampered and must fail closed.
var errNotAPack = errors.New("not a pack")

// loadPack reads a pack from a directory. A missing pack.toml is an error (the
// presence of pack.toml is the entire "is this a pack" test), wrapped around
// errNotAPack so callers can tell "absent" from "broken".
func loadPack(root string) (*packInfo, error) {
	root = filepath.Clean(root)
	mf := filepath.Join(root, packManifestName)
	b, err := os.ReadFile(mf)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is %w (no %s)", root, errNotAPack, packManifestName)
		}
		return nil, err
	}
	var m packManifest
	if err := toml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", mf, err)
	}
	p := &packInfo{Root: root, Manifest: m}
	if d := filepath.Join(root, "skills"); dirHasEntries(d) {
		if isSymlinkPath(d) {
			return nil, fmt.Errorf("pack %s: skills/ is a symlink; refusing to mount", root)
		}
		if has, bad := dirHasSymlink(d); has {
			return nil, fmt.Errorf("pack %s: skills/ contains a symlink (%s); packs must not use symlinks, refusing to mount", root, bad)
		}
		p.SkillsDir = d
	}
	if d := filepath.Join(root, "knowledge"); dirHasEntries(d) {
		if isSymlinkPath(d) {
			return nil, fmt.Errorf("pack %s: knowledge/ is a symlink; refusing to mount", root)
		}
		if has, bad := dirHasSymlink(d); has {
			return nil, fmt.Errorf("pack %s: knowledge/ contains a symlink (%s); packs must not use symlinks, refusing to mount", root, bad)
		}
		p.KnowledgeDir = d
	}
	if d := filepath.Join(root, "bin"); dirHasEntries(d) {
		if isSymlinkPath(d) {
			return nil, fmt.Errorf("pack %s: bin/ is a symlink; refusing to mount", root)
		}
		if has, bad := dirHasSymlink(d); has {
			return nil, fmt.Errorf("pack %s: bin/ contains a symlink (%s); packs must not use symlinks, refusing to mount", root, bad)
		}
		p.BinDir = d
	}
	if f := filepath.Join(root, "capabilities.json"); fileExists(f) {
		if isSymlinkPath(f) {
			return nil, fmt.Errorf("pack %s: capabilities.json is a symlink; refusing to mount", root)
		}
		p.CapabilitiesFile = f
	}
	if err := validatePackFacets(root, &m); err != nil {
		return nil, err
	}
	return p, nil
}

// validatePackFacets hardens the v2 typed facets at load time (fail closed,
// same posture as the existing skills/knowledge symlink checks): every
// proxy/bin/knowledge Name must be a safe artifact name (reusing
// safeArtifactName); every [[bin]].Path must be a repo-relative path that does
// not escape the pack root and is not a symlink; every [[bin]] MUST carry a
// non-empty SHA (an external binary is never registered unpinned — P2 never
// reaches an exec path for one that failed to load). [[knowledge]].Source is
// deliberately NOT root-scoped: a shared=false reference pointing OUTSIDE the
// pack (e.g. ~/notes/okf) is the entire point of a private reference (F6).
func validatePackFacets(root string, m *packManifest) error {
	for _, p := range m.Proxies {
		if !safeArtifactName(p.Name) {
			return fmt.Errorf("pack %s: [[proxy]] name %q is invalid (letters, digits, -, _, . only; no path separators)", root, p.Name)
		}
	}
	for _, b := range m.Bins {
		if !safeArtifactName(b.Name) {
			return fmt.Errorf("pack %s: [[bin]] name %q is invalid (letters, digits, -, _, . only; no path separators)", root, b.Name)
		}
		if strings.TrimSpace(b.SHA) == "" {
			return fmt.Errorf("pack %s: [[bin]] %q has no sha — external binaries must be SHA-pinned (fail closed)", root, b.Name)
		}
		if err := validateRepoRelativePath(root, b.Path); err != nil {
			return fmt.Errorf("pack %s: [[bin]] %q: %w", root, b.Name, err)
		}
	}
	for _, k := range m.Knowledge {
		if !safeArtifactName(k.Name) {
			return fmt.Errorf("pack %s: [[knowledge]] name %q is invalid (letters, digits, -, _, . only; no path separators)", root, k.Name)
		}
		if strings.TrimSpace(k.Source) == "" {
			return fmt.Errorf("pack %s: [[knowledge]] %q has no source", root, k.Name)
		}
		// CRITICAL (finding A): the shared flag MUST match the source's CLASS.
		// shared=true ("travels") REQUIRES a git URL — resolved through the
		// safeGitURL-gated clone path; shared=false ("private") REQUIRES a
		// local path. Without this, an adopted pack could declare shared=true
		// with a LOCAL path (e.g. "/etc" or ~/.ssh) and bypass the adopted-pack
		// guard in resolvePackKnowledgeRef, which the flag alone used to key —
		// host-file disclosure via the knowledge index. Fail closed at load.
		if k.Shared && !knowledgeSourceIsGitURL(k.Source) {
			return fmt.Errorf("pack %s: [[knowledge]] %q: shared=true requires a git URL source (got local path %q); use shared=false for a local path", root, k.Name, k.Source)
		}
		if !k.Shared && knowledgeSourceIsGitURL(k.Source) {
			return fmt.Errorf("pack %s: [[knowledge]] %q: shared=false (private) requires a local path source (got URL %q); use shared=true for a git URL", root, k.Name, k.Source)
		}
	}
	return nil
}

// knowledgeSourceIsGitURL classifies a [[knowledge]].Source as git-URL-shaped
// (cloneable — including transport-helper strings like ext::/fd:: that must
// route through the safeGitURL rejection rather than be mistaken for a local
// path) vs a local path. The finding-A security guards key on this CLASS,
// never on the manifest's shared flag, which an attacker controls.
func knowledgeSourceIsGitURL(source string) bool {
	source = strings.TrimSpace(source)
	return isGitURL(source) || strings.Contains(source, "::")
}

// validateRepoRelativePath rejects a [[bin]].Path that is empty, absolute, that
// escapes root via `..`, or that resolves to a symlink — mirroring the
// skills/knowledge symlink posture. rel MUST be repo-relative (packs-v2-impl.md
// §2: "path = bin/fastmail-mcp").
func validateRepoRelativePath(root, rel string) error {
	if strings.TrimSpace(rel) == "" {
		return fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(rel) {
		return fmt.Errorf("path %q must be repo-relative, not absolute", rel)
	}
	clean := filepath.Join(root, rel)
	if !strings.HasPrefix(clean, filepath.Clean(root)+string(filepath.Separator)) {
		return fmt.Errorf("path %q escapes the pack root", rel)
	}
	if isSymlinkPath(clean) {
		return fmt.Errorf("path %q is a symlink; refusing to mount", rel)
	}
	return nil
}

// dirHasSymlink walks dir and reports the first symlink of ANY kind. Adopted
// packs have no legitimate need for symlinks, and rejecting all of them is the
// only complete defense: WalkDir does NOT descend into a symlinked DIRECTORY, so
// an "escaping vs not" test can be masked (skills/sub -> ../masked, then
// masked/x -> /etc). Rejecting the link entry itself (which WalkDir DOES visit)
// closes that bypass. Returns (true, path) on the first symlink found.
// isSymlinkPath reports whether path itself is a symlink (Lstat, no follow).
func isSymlinkPath(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

func dirHasSymlink(dir string) (bool, string) {
	bad := ""
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			bad = path
			return filepath.SkipAll
		}
		return nil
	})
	return bad != "", bad
}

// activePackRoot resolves the active pack path: the --pack override wins, else
// config's `pack`. "" means no active pack.
func activePackRoot(cfgPack, override string) string {
	if strings.TrimSpace(override) != "" {
		return expandUser(strings.TrimSpace(override))
	}
	return expandUser(strings.TrimSpace(cfgPack))
}

// expandUser expands a leading ~ to $HOME (git/toml don't do it for us).
func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// defaultPackRoot is the default-pack location. It runs the legacy-dir
// migration (the "pack"/"personal" -> "default" rename) once per resolution
// so every call site that resolves the default pack picks up an existing
// user's pack without a second copy of the migration logic.
//
// FAIL CLOSED on a failed migration: a failed (rolled-back) migration leaves
// the user's real pack at the LEGACY path, so this returns that legacy path
// rather than the nonexistent default root — returning the empty default
// would make callers (setup's ensure-default-pack, `pack new`) create a
// SECOND empty pack beside the real one and strand cfg.Pack.
func defaultPackRoot() string {
	if err := migrateLegacyPackDirErr(); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack: pack migration: %v\n", err)
		if legacy := existingLegacyPackRoot(); legacy != "" {
			return legacy
		}
	}
	return config.PackDir()
}

// migrateLegacyPackDir renames an existing legacy default-pack dir to the new
// ".../default" one (config.PackDir()'s basename), so an existing user is
// never orphaned across either rename ("pack" -> "personal" -> "default").
// Best-effort and cheap to call repeatedly: once ".../default" exists
// (freshly created or already migrated) it's a no-op after one stat.
//
// Preference when BOTH a legacy ".../personal" and an older ".../pack" exist:
// ".../personal" wins (it's the more recent name) and ".../pack" is left
// untouched — migrating both would be ambiguous, and silently deleting/merging
// the older one could lose a user's data.
//
// Symlink-safe: Lstat (not Stat) each legacy candidate so a SYMLINKED legacy
// dir is refused rather than followed into a rename that could escape the data
// dir (a plain os.Rename on a symlink would rename the link itself, silently
// separating it from its target and potentially placing "default" at an
// attacker-chosen location if the link were ever attacker-controlled).
//
// The migration is ONE TRANSACTION under a single hold of the pack-trust
// flock (withPackTrustLock): directory rename, manifest `name` rewrite,
// trust-store path-state migration (migratePackTrustPaths), and cfg.Pack
// old->new, in that order, every persist error CHECKED. On any failure it
// rolls the already-applied steps back as far as safely possible (config ->
// trust -> manifest -> directory) and returns a loud error; defaultPackRoot
// then fails CLOSED to the legacy path, so a failed migration never strands
// cfg.Pack pointing at a gone dir and never lets a caller create a second
// empty pack at the default root.
func migrateLegacyPackDirErr() error {
	newDir := config.PackDir()
	if _, err := os.Stat(newDir); err == nil {
		// Already migrated (or a fresh "default" pack exists). Repair any state
		// stranded by the OLD, non-transactional migration (which ignored
		// cfg.Save errors and never migrated trust state).
		repairStaleLegacyPackState(newDir)
		return nil
	}
	oldDir := existingLegacyPackRoot()
	if oldDir == "" {
		return nil // no legacy dir to migrate
	}
	return withPackTrustLock(func() error {
		// Re-check under the lock: a concurrent invocation may have migrated
		// while we were waiting for it.
		if _, err := os.Stat(newDir); err == nil {
			return nil
		}
		return migrateLegacyPackDirLocked(oldDir, newDir)
	})
}

// savePackMigrationConfig is the config-persist seam the pack migration goes
// through (a package var so tests can force a save failure and assert the
// rollback without breaking config.Save globally).
var savePackMigrationConfig = func(c *config.Config) error { return c.Save() }

// migrateLegacyPackDirLocked is the transactional core of the legacy-pack
// migration. The caller MUST hold withPackTrustLock (it touches the trust
// store directly, never via mutatePackTrustStore, so the flock is never
// nested). Order + rollback:
//
//  0. snapshot: config + trust store are loaded (and the trust store
//     deep-copied via JSON) BEFORE anything is touched; a snapshot failure
//     aborts the migration outright (a migration that cannot guarantee its
//     own rollback never starts).
//  1. directory: os.Rename old -> new (plain rename preserves .git).
//  2. manifest: Name -> "default", every other field preserved.
//  3. trust store: every PATH-KEYED record (accepted keys + record paths,
//     adoption keys, installed-set owner, activation path/owner) follows the
//     rename; remote-keyed identity is stable and stays put. Saved with the
//     error checked.
//  4. config: cfg.Pack old -> new, saved with the error checked.
//
// A failure at step N rolls back steps N-1..1 (best-effort, each rollback
// failure reported in the returned error) so the migration is all-or-nothing
// as far as the filesystem allows.
func migrateLegacyPackDirLocked(oldDir, newDir string) error {
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		return fmt.Errorf("loading config before migration: %w (cannot verify cfg.pack; leaving %s in place)", cfgErr, oldDir)
	}
	trust, terr := loadPackTrustStore()
	if terr != nil {
		return fmt.Errorf("loading pack-trust store before migration: %w (leaving %s in place)", terr, oldDir)
	}
	trustSnapshot, serr := snapshotPackTrustStore(trust)
	if serr != nil {
		return fmt.Errorf("snapshotting pack-trust store before migration: %w (leaving %s in place)", serr, oldDir)
	}
	oldCanon := canonicalizePackRoot(oldDir)
	newCanon := canonicalizePackRoot(newDir)

	// cfg.Pack ALIAS guard: canonicalizePackRoot is Abs+Clean only (never
	// EvalSymlinks — see its own doc comment), so a cfg.Pack that is itself a
	// DIFFERENT path which happens to be a SYMLINK resolving onto oldDir will
	// never string-match oldCanon in step 4 below, even though it is, on disk,
	// exactly the same pack. Renaming oldDir out from under such an alias would
	// leave cfg.Pack pointing at a now-dangling symlink, and the trust store's
	// alias-keyed state would never get migrated either (it also keys off the
	// alias path, not oldCanon). Detect this BEFORE touching anything —
	// EvalSymlinks(cfg.Pack) resolves to oldDir but the canonical STRINGS differ
	// — and refuse the whole migration outright: no rename, no manifest edit, no
	// trust-store change, no config write. defaultPackRoot's caller already
	// falls back to the legacy path on any migration error, which is exactly
	// right here (the alias's target keeps its original name).
	if alias := strings.TrimSpace(cfg.Pack); alias != "" {
		aliasCanon := canonicalizePackRoot(alias)
		if aliasCanon != oldCanon {
			if resolved, rerr := filepath.EvalSymlinks(alias); rerr == nil && canonicalizePackRoot(resolved) == oldCanon {
				return fmt.Errorf("cfg.pack %q is a symlink alias resolving to %s, but its own path does not match — refusing to migrate (would leave the alias dangling); repoint cfg.pack directly at %s first, or remove the alias", alias, oldDir, oldDir)
			}
		}
	}

	// 1) directory. Preserve git history: a plain rename keeps .git intact.
	if err := os.Rename(oldDir, newDir); err != nil {
		return fmt.Errorf("renaming %s to %s: %w", oldDir, newDir, err)
	}
	rollbackDir := func() error { return os.Rename(newDir, oldDir) }

	// 2) manifest.
	prevName, merr := renamePackManifestToDefault(newDir)
	if merr != nil {
		if back := rollbackDir(); back != nil {
			return fmt.Errorf("renamed %s to %s but could not update its manifest (%v), AND the directory rollback failed (%v) — fix manually", oldDir, newDir, merr, back)
		}
		return fmt.Errorf("could not rename pack manifest to %q (%v); migration rolled back, %s left in place", "default", merr, oldDir)
	}
	rollbackManifest := func() error { return setPackManifestName(newDir, prevName) }

	// 3) trust store: path-keyed state follows the rename. Saved directly (the
	// caller holds the lock; mutatePackTrustStore here would nest the flock).
	trustChanged := migratePackTrustPaths(trust, oldCanon, newCanon)
	if trustChanged {
		if err := trust.save(); err != nil {
			msgs := []string{fmt.Sprintf("could not save migrated pack-trust store: %v", err)}
			if back := rollbackManifest(); back != nil {
				msgs = append(msgs, fmt.Sprintf("manifest rollback failed: %v", back))
			}
			if back := rollbackDir(); back != nil {
				msgs = append(msgs, fmt.Sprintf("directory rollback failed: %v", back))
			}
			return fmt.Errorf("pack migration failed and was rolled back: %s", strings.Join(msgs, "; "))
		}
	}
	rollbackTrust := func() error {
		if !trustChanged {
			return nil
		}
		return restorePackTrustSnapshot(trustSnapshot)
	}

	// 4) config: cfg.Pack old -> new, error CHECKED (the old code ignored the
	// save error and could strand cfg.Pack pointing at the gone legacy dir).
	if cfg.Pack == oldDir || (strings.TrimSpace(cfg.Pack) != "" && canonicalizePackRoot(cfg.Pack) == oldCanon) {
		cfg.Pack = newDir
		if err := savePackMigrationConfig(cfg); err != nil {
			msgs := []string{fmt.Sprintf("could not save cfg.pack %s -> %s: %v", oldDir, newDir, err)}
			if back := rollbackTrust(); back != nil {
				msgs = append(msgs, fmt.Sprintf("trust-store rollback failed: %v", back))
			}
			if back := rollbackManifest(); back != nil {
				msgs = append(msgs, fmt.Sprintf("manifest rollback failed: %v", back))
			}
			if back := rollbackDir(); back != nil {
				msgs = append(msgs, fmt.Sprintf("directory rollback failed: %v", back))
			}
			return fmt.Errorf("pack migration failed and was rolled back: %s", strings.Join(msgs, "; "))
		}
	}
	return nil
}

// legacyPackCandidates returns the legacy default-pack paths in preference
// order ("personal" — the more recent legacy name — before the original
// "pack"). Empty when the data dir can't be resolved.
func legacyPackCandidates() []string {
	dataDir, err := config.DataDir()
	if err != nil {
		return nil
	}
	return []string{
		filepath.Join(dataDir, "personal"), // preferred: the more recent legacy name
		filepath.Join(dataDir, "pack"),     // original legacy name
	}
}

// existingLegacyPackRoot returns the first migratable legacy pack dir
// (non-symlinked real pack), or "" when none exists.
func existingLegacyPackRoot() string {
	for _, c := range legacyPackCandidates() {
		if isMigratableLegacyPackDir(c) {
			return c
		}
	}
	return ""
}

// migratePackTrustPaths rewrites every PATH-KEYED piece of trust state from
// oldCanon to newCanon: Accepted keys "path:<old>" (including legacy
// commit-suffixed "path:<old>#<commit>" keys) and each record's Path field,
// Adopted map keys, Installed.Owner (only a "path:" owner — remote identity
// is stable across a directory rename), and Activation.Path/Owner (same
// remote caveat). Fingerprints, provenance metadata, wrapper lists, and
// contribution sets are preserved bit-for-bit; a remote-keyed accepted
// record only gets its Path metadata refreshed. Reports whether anything
// changed. Pure in-memory: the caller persists.
func migratePackTrustPaths(s *packTrustStore, oldCanon, newCanon string) bool {
	if s == nil || strings.TrimSpace(oldCanon) == "" || oldCanon == newCanon {
		return false
	}
	changed := false
	oldKey, newKey := "path:"+oldCanon, "path:"+newCanon
	if len(s.Accepted) > 0 {
		next := make(map[string]packTrustRecord, len(s.Accepted))
		for k, rec := range s.Accepted {
			nk := k
			if k == oldKey {
				nk = newKey
			} else if strings.HasPrefix(k, oldKey+"#") { // legacy commit-suffixed key
				nk = newKey + strings.TrimPrefix(k, oldKey)
			}
			if rec.Path == oldCanon {
				rec.Path = newCanon
				changed = true
			}
			if nk != k {
				changed = true
			}
			next[nk] = rec
		}
		s.Accepted = next
	}
	if prov, ok := s.Adopted[oldCanon]; ok {
		delete(s.Adopted, oldCanon)
		s.Adopted[newCanon] = prov
		changed = true
	}
	if s.Installed != nil && s.Installed.Owner == oldKey {
		s.Installed.Owner = newKey
		changed = true
	}
	if s.Activation != nil {
		if s.Activation.Path == oldCanon {
			s.Activation.Path = newCanon
			changed = true
		}
		if s.Activation.Owner == oldKey {
			s.Activation.Owner = newKey
			changed = true
		}
	}
	return changed
}

// snapshotPackTrustStore returns a JSON snapshot of s suitable for
// restorePackTrustSnapshot, so a caller can persist a mutation and still roll
// it back if a LATER step in the same transaction fails.
func snapshotPackTrustStore(s *packTrustStore) ([]byte, error) {
	return json.Marshal(s)
}

// restorePackTrustSnapshot writes back a snapshot produced by
// snapshotPackTrustStore, undoing an already-persisted trust-store save when a
// later step of the same transaction (e.g. the paired config save) fails.
// Shared by migrateLegacyPackDirLocked and repairStaleLegacyPackState so both
// trust-then-config transactions roll back identically.
func restorePackTrustSnapshot(snapshot []byte) error {
	var snap packTrustStore
	if err := json.Unmarshal(snapshot, &snap); err != nil {
		return err
	}
	return snap.save()
}

// repairStaleLegacyPackState handles the aftermath of the OLD,
// non-transactional migration (directory + manifest renamed, but the cfg.Save
// error ignored and trust path-state never migrated): once ".../default"
// exists, a cfg.Pack still pointing EXACTLY at a legacy personal/pack path
// that is GONE is repointed at newDir, and trust-store path state still keyed
// by a gone legacy path is migrated. Idempotent, and deliberately
// conservative: a legacy path that still EXISTS on disk is a real, live pack
// the user may be using — never hijacked. Best-effort in the sense that a
// repair failure never blocks resolution (the default dir is present and
// usable either way) — but NOT best-effort about leaving inconsistent state
// behind: trust and config are saved as ONE transaction (snapshot before
// touching either, save trust, then save config with its error CHECKED; a cfg
// save failure rolls the just-persisted trust save back), so a failure never
// leaves the trust store migrated while cfg.Pack still points at the stale
// path (or vice versa) — the old bug this function exists to fix in the first
// place, now also guarded against in ITS OWN repair path. A repair failure is
// reported to stderr; the next call retries from scratch (idempotent probes).
//
// legacyRepairMightBeNeeded runs an UNLOCKED, cheap probe purely to skip the
// flock in the steady state (nothing stale). It is a SKIP-ONLY decision: a
// probe load error or a race is never treated as "no need" (that was the old
// bug — a preliminary decision made outside the lock, off a possibly stale or
// errored read, was carried straight into the locked section instead of being
// recomputed). Every needCfg/needTrust decision that actually drives a
// mutation is recomputed FRESH, under the lock, in repairStaleLegacyPackStateLocked
// — including which legacy dirs are still absent, so a legacy dir that came
// back to life while this call was waiting for the lock is left untouched.
func repairStaleLegacyPackState(newDir string) {
	newCanon := canonicalizePackRoot(newDir)
	if !legacyRepairMightBeNeeded(newCanon) {
		return
	}
	if err := withPackTrustLock(func() error {
		return repairStaleLegacyPackStateLocked(newDir, newCanon)
	}); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack: pack migration repair: %v\n", err)
	}
}

// legacyRepairMightBeNeeded is the unlocked preflight: it may say "nothing to
// do" only when it can POSITIVELY confirm that (every legacy candidate still
// exists, or both stores loaded clean and neither needs a change). Anything
// uncertain — a config/trust load error, or an actual pending change — returns
// true so the caller takes the lock and lets repairStaleLegacyPackStateLocked
// make the real, fresh decision.
func legacyRepairMightBeNeeded(newCanon string) bool {
	var stale []string
	for _, c := range legacyPackCandidates() {
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			stale = append(stale, c)
		}
	}
	if len(stale) == 0 {
		return false
	}
	cfg, cerr := config.Load()
	if cerr != nil {
		return true // can't rule it out; let the locked path decide
	}
	if legacyPathIsAmong(cfg.Pack, stale) {
		return true
	}
	s, terr := loadPackTrustStore()
	if terr != nil {
		return true
	}
	for _, c := range stale {
		// Probe on the in-memory copy only; nothing is saved here.
		if migratePackTrustPaths(s, canonicalizePackRoot(c), newCanon) {
			return true
		}
	}
	return false
}

// legacyPathIsAmong reports whether cfgPack (cfg.Pack) names one of the given
// legacy candidate paths, by exact string or canonical-path match.
func legacyPathIsAmong(cfgPack string, candidates []string) bool {
	if strings.TrimSpace(cfgPack) == "" {
		return false
	}
	for _, c := range candidates {
		if cfgPack == c || canonicalizePackRoot(cfgPack) == canonicalizePackRoot(c) {
			return true
		}
	}
	return false
}

// repairStaleLegacyPackStateLocked is the LOCKED core of
// repairStaleLegacyPackState. The caller MUST hold withPackTrustLock. Nothing
// decided before the lock was taken (the cheap preflight) is trusted here:
// which legacy dirs are absent, and whether config/trust actually need a
// change, are all recomputed against a FRESH load taken under the lock — so a
// concurrent repair (or a legacy dir that reappeared while this call waited
// for the lock) can never be raced into a one-sided mutation, and a load
// error here aborts the WHOLE repair (never a partial needCfg/needTrust
// decision made off a failed read) rather than silently treating the load
// failure as "nothing to do". The caller logs the error and retries next time
// (idempotent probes, so a transient load failure is never fatal to the
// user).
func repairStaleLegacyPackStateLocked(newDir, newCanon string) error {
	var stale []string // legacy candidates that no longer exist on disk, as of NOW
	for _, c := range legacyPackCandidates() {
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			stale = append(stale, c)
		}
	}
	if len(stale) == 0 {
		// Every legacy candidate is live again (e.g. re-created while this call
		// waited for the lock) — do nothing for it.
		return nil
	}
	cfg, cerr := config.Load()
	if cerr != nil {
		return fmt.Errorf("loading config for repair: %w", cerr)
	}
	trust, terr := loadPackTrustStore()
	if terr != nil {
		return fmt.Errorf("loading pack-trust store for repair: %w", terr)
	}
	needCfg := legacyPathIsAmong(cfg.Pack, stale)
	// Snapshot BEFORE mutating the freshly-loaded trust object, so a later
	// cfg-save failure can still roll back an already-persisted trust save.
	trustSnapshot, serr := snapshotPackTrustStore(trust)
	if serr != nil {
		return fmt.Errorf("snapshotting pack-trust store before repair: %w", serr)
	}
	needTrust := false
	for _, c := range stale {
		if migratePackTrustPaths(trust, canonicalizePackRoot(c), newCanon) {
			needTrust = true
		}
	}
	if !needCfg && !needTrust {
		return nil
	}
	trustSaved := false
	if needTrust {
		if err := trust.save(); err != nil {
			return err
		}
		trustSaved = true
	}
	// failCheckedRollback wraps a step error, rolling back an already-saved
	// trust mutation first (reporting BOTH failures if the rollback itself
	// fails) so a cfg-save failure after a successful trust save never leaves
	// the two out of sync — this is the transactional guarantee this function
	// exists to add.
	failCheckedRollback := func(stepErr error) error {
		if !trustSaved {
			return stepErr
		}
		if back := restorePackTrustSnapshot(trustSnapshot); back != nil {
			return fmt.Errorf("%w (AND trust-store rollback failed: %v)", stepErr, back)
		}
		return fmt.Errorf("%w (trust-store change rolled back)", stepErr)
	}
	if needCfg {
		cfg.Pack = newDir
		if serr := savePackMigrationConfig(cfg); serr != nil {
			return failCheckedRollback(serr)
		}
	}
	return nil
}

// isMigratableLegacyPackDir reports whether dir is a real, non-symlinked pack
// (has pack.toml) safe to rename — the shared guard both migration candidates
// go through.
func isMigratableLegacyPackDir(dir string) bool {
	fi, lerr := os.Lstat(dir)
	if lerr != nil || fi.Mode()&os.ModeSymlink != 0 {
		return false // absent, or a symlink — refuse to follow it
	}
	if !fi.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, packManifestName)); err != nil {
		return false // not actually a pack (no pack.toml)
	}
	return true
}

// renamePackManifestToDefault loads root's manifest and rewrites its Name to
// "default", preserving every other field, then writes it back via the
// leaf-symlink-safe atomic writePackManifest. A no-op if Name is already
// "default". Returns the PREVIOUS name so the transactional migration can
// roll the rewrite back.
func renamePackManifestToDefault(root string) (string, error) {
	p, err := loadPack(root)
	if err != nil {
		return "", err
	}
	prev := p.Manifest.Name
	if prev == "default" {
		return prev, nil
	}
	p.Manifest.Name = "default"
	if err := writePackManifest(root, p.Manifest); err != nil {
		return prev, err
	}
	return prev, nil
}

// setPackManifestName rewrites root's manifest Name to name, preserving every
// other field (the rollback half of renamePackManifestToDefault).
func setPackManifestName(root, name string) error {
	p, err := loadPack(root)
	if err != nil {
		return err
	}
	if p.Manifest.Name == name {
		return nil
	}
	p.Manifest.Name = name
	return writePackManifest(root, p.Manifest)
}

// applyPackToLaunch mounts the active pack into a launch (run OR task): it
// appends the pack's skills dir to o.Skills, applies the pack's ollama model
// pref, synthesizes + stacks the pack's sandbox bin/ mixin kit (F2), and warns
// about a missing integration credential. A pack's `integration.mcp` is NOT
// warned about here (v1 behavior): F1 enables it into cfg.MCP at `pack use`
// time, so buildSbxArgs' existing --mcp loop already attaches it on the next
// create — nothing new needed in the arg builder, and warning here would be
// stale noise for an already-attached pack. Knowledge is NOT handled here
// either: a persisted active pack already has its bundles (embedded dir AND
// [[knowledge]] refs) in cfg.KnowledgeBundles (added at `pack use` time,
// indexed by the daemon), and a transient --pack override's knowledge is
// deliberately not scoped (the daemon wouldn't have indexed it). An EXPLICIT
// --pack that fails to load is fatal (a non-nil return the caller must treat
// as launch-aborting). The CONFIGURED active pack (cfg.Pack, or the default
// pack fallback) fails CLOSED too when it exists but won't load — a symlink
// rejection, facet-validation failure, or parse error means a broken or
// TAMPERED pack, and launching without its declared wrappers/skills would be
// a silent downgrade. The ONLY degradable case is errNotAPack ("genuinely
// absent": the dir or its pack.toml is gone), which warns once and proceeds
// as if no pack were active. A declared-but-unbuildable sandbox proxy is ALSO
// fatal (round-4 F2): the launch fails CLOSED rather than creating a sandbox
// missing a declared wrapper.
//
// It returns the EFFECTIVE active-pack root it actually applied: "" when there
// is no active pack, OR when it degraded via errNotAPack (genuinely absent —
// see below); the real packRoot when a pack loaded and its context was mounted
// onto o/cfg. Callers (run.go, task.go) MUST write the sandbox.pack marker and
// scope memory from this returned root, never from activePackRoot(cfg.Pack,
// o.Pack) directly — the configured path can name a pack that degraded to
// pack-less this launch, and recording it anyway would make a later
// stalePackReattachWarning wrongly stay silent (marker == active) even though
// the sandbox never got the pack's create-time facets.
func applyPackToLaunch(cfg *config.Config, o *runOpts, env shellEnv) (string, error) {
	packRoot := activePackRoot(cfg.Pack, o.Pack)
	if packRoot == "" {
		return "", nil // no active pack; nothing to mount (detached or never created)
	}
	p, err := loadPack(packRoot)
	if err != nil {
		if strings.TrimSpace(o.Pack) != "" {
			return "", fmt.Errorf("--pack %s: %v", o.Pack, err)
		}
		if errors.Is(err, errNotAPack) {
			// Genuinely absent (deleted dir / no pack.toml): warn and launch
			// without it, as if no pack were active. Not fatal — a stale
			// cfg.Pack must not brick every launch. The pack did NOT apply, so
			// the effective root is "" — the caller must not mark this launch
			// as having this pack.
			fmt.Fprintf(os.Stderr, "pi-stack: active pack unavailable (%v); launching without it — `pi-stack pack use <path>` to re-point it or `pi-stack pack rm` to detach\n", err)
			return "", nil
		}
		// The pack EXISTS but won't load (symlink injected, validation/parse
		// failure): fail the launch closed. Creating a sandbox from a broken or
		// tampered active pack would silently drop its declared context.
		return "", fmt.Errorf("active pack %s: %v (refusing to launch without the pack's declared context; fix the pack or `pi-stack pack rm` to detach it)", packRoot, err)
	}
	if p.SkillsDir != "" && !containsStr(o.Skills, p.SkillsDir) {
		o.Skills = append(o.Skills, p.SkillsDir)
	}
	if m := strings.TrimSpace(p.Manifest.OllamaBridgeModel); m != "" {
		cfg.OllamaBridgeModel = m
	}
	// F2: synthesize (or refresh) the ephemeral mixin kit carrying this pack's
	// non-host bin/ wrappers, and stack it via the DEDICATED PackKits field (never
	// o.Kits — see the PackKits field doc for why folding it into the --kit
	// escape hatch would silently drop the base image kit). FAIL CLOSED at the
	// launch boundary (round-4 F2): a pack that DECLARES a sandbox proxy whose
	// kit can't be built must abort the launch — synthesizePackKit distinguishes
	// "no proxies declared" (("", nil): fine, no kit) from "declared but
	// unbuildable" (("", err)), so a sandbox is never created silently missing a
	// wrapper the pack promised it.
	kit, kerr := synthesizePackKit(p)
	if kerr != nil {
		return "", fmt.Errorf("pack %s: %v (refusing to launch a sandbox missing a declared wrapper; fix the pack's bin/ or drop the [[proxy]] entry)", p.Manifest.Name, kerr)
	}
	if kit != "" && !containsStr(o.PackKits, kit) {
		o.PackKits = append(o.PackKits, kit)
	}
	for _, ig := range p.Manifest.Integrations {
		// CONTAINER integrations (Manifest set) get their credentials Docker-side,
		// not from op-refs — so an op-ref warning would be misleading noise. Only
		// warn for op-run-wrapped (host-provided/remote) integrations.
		if ig.Env != "" && ig.Manifest == "" && !opRefFilled(env, ig.Env) {
			fmt.Fprintf(os.Stderr, "pi-stack: pack integration %q needs a credential — set it: pi-stack secret set %s op://vault/item/field\n", ig.Name, ig.Env)
		}
	}
	// S01: every pack integration's MCP server is in the preload set — no more
	// eager/lazy split. `pack use` already persists each integration's server
	// into cfg.MCP (packMcpNames/F1, above), so this only matters for a
	// TRANSIENT --pack override that was never `pack use`d: fold its
	// integration names into cfg.MCP IN MEMORY for this launch only. Never
	// Save()d — run.go/task.go never call Save() on this cfg after
	// applyPackToLaunch, so a --pack override never leaks into the persisted
	// config.
	for _, n := range packMcpNames(p) {
		if !containsStr(cfg.MCP, n) {
			cfg.MCP = append(cfg.MCP, n)
		}
	}
	return packRoot, nil
}

// packContainerMCP returns {integration.mcp: packContainer} for a pack's
// CONTAINER/REMOTE integrations — Manifest servers (`sbx mcp add <name> --local
// --url`), Image servers (`docker run <image>`, op-run wrapped), and remote URL
// servers (`sbx mcp add <name> --url`, OAuth'd host-side). These are what
// `pi-stack mcp register` adds specially rather than as a plain host subcommand.
// Returns nil when the pack declares none.
func packContainerMCP(p *packInfo) map[string]packContainer {
	out := map[string]packContainer{}
	for _, ig := range p.Manifest.Integrations {
		if ig.MCP == "" {
			continue
		}
		switch {
		case strings.TrimSpace(ig.Manifest) != "":
			out[ig.MCP] = packContainer{Manifest: strings.TrimSpace(ig.Manifest)}
		case strings.TrimSpace(ig.Image) != "":
			var keys []string
			if ig.Env != "" {
				keys = append(keys, ig.Env) // the op-refs secret, forwarded too
			}
			keys = append(keys, ig.EnvKeys...)
			out[ig.MCP] = packContainer{Image: strings.TrimSpace(ig.Image), EnvKeys: keys}
		case strings.TrimSpace(ig.URL) != "":
			out[ig.MCP] = packContainer{RemoteURL: strings.TrimSpace(ig.URL)}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// activeContainerMCP resolves packContainerMCP for the active pack. Returns nil
// when there is no active pack or it won't load (registration of the other
// servers proceeds regardless).
func activeContainerMCP(cfg *config.Config) map[string]packContainer {
	root := activePackRoot(cfg.Pack, "")
	if root == "" {
		return nil
	}
	p, err := loadPack(root)
	if err != nil {
		return nil
	}
	return packContainerMCP(p)
}

// packMcpNames returns the de-duplicated `integration.mcp` names a pack
// declares, in manifest order. Used by runPackUse to compute what F1 attaches.
func packMcpNames(p *packInfo) []string {
	var names []string
	seen := map[string]bool{}
	for _, ig := range p.Manifest.Integrations {
		if ig.MCP != "" && !seen[ig.MCP] {
			seen[ig.MCP] = true
			names = append(names, ig.MCP)
		}
	}
	return names
}

// packKitDir resolves the PER-PACK KEY under which a pack's ephemeral mixin
// kits are synthesized: <StateDir>/pi-stack/pack-kits/<hash>, keyed by a hash
// of the pack root. Since round-3 R2 this is a naming PREFIX, not a live dir:
// every launch synthesizes into its own unique <hash>.kit-XXXX dir beside it
// (see synthesizePackKit), and sweepStaleKitTemps age-gates the cleanup of old
// ones (a `pi-stack state reset` sweep is the backstop).
func packKitDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	dir, err := config.StateDir()
	if err != nil {
		dir = "pi-stack-state"
	}
	return filepath.Join(dir, "pi-stack", "pack-kits", hex.EncodeToString(sum[:])[:16])
}

// synthesizePackKit builds the ephemeral mixin kit that puts a pack's non-host
// bin/ wrappers on the sandbox PATH (F2/ADR-2): a minimal `kind: mixin`
// spec.yaml, plus files/home/.local/bin/<name> (0755) for each [[proxy]] with
// Host unset/false — ~/.local/bin is on the sandbox PATH and (unlike /usr/local)
// is reached by the runtime mixin-kit mount. Returns (dir, nil) on success, ("", nil) when
// the pack has no sandbox proxies (nothing to mount — the caller must not
// stack an empty kit), and ("", err) when the pack DECLARES a sandbox proxy
// but the kit can't be built — the caller must fail the launch closed
// (round-4 F2), never proceed to a kitless create.
// Copies (never symlinks): loadPack already refuses a symlinked bin/, and sbx
// mounts a real tree.
//
// PER-LAUNCH UNIQUE DIR (round-3 R2): every call synthesizes into its OWN
// os.MkdirTemp dir (keyed by the pack hash as a name prefix) and returns THAT
// path for --kit. There is no stable shared path any more, so there is no
// replace-in-place window where the live kit is briefly absent, and two
// concurrent launches of the same pack can never clash on a shared mutable
// dir — each builds its kit COMPLETELY before returning it, then never touches
// it again. A proxy removed from pack.toml can't resurrect either: the fresh
// dir only ever holds what THIS synth wrote (the old finding-#6 guarantee,
// now structural). Old launch dirs are age-gate swept (sweepStaleKitTemps).
// And it FAILS CLOSED: if any declared wrapper can't be read or copied, the
// whole synth is refused with an error — never a partial kit with that one
// wrapper silently missing.
func synthesizePackKit(p *packInfo) (string, error) {
	var sandboxProxies []packProxy
	for _, pr := range p.Manifest.Proxies {
		if !pr.Host {
			sandboxProxies = append(sandboxProxies, pr)
		}
	}
	base := packKitDir(p.Root)
	parent := filepath.Dir(base)
	sweepStaleKitTemps(parent, filepath.Base(base))
	if len(sandboxProxies) == 0 && p.CapabilitiesFile == "" {
		// No sandbox proxies and no capabilities.json: nothing to mount. A previous
		// launch's kit dir is inert (nothing references it) and the sweep above
		// cleans it up.
		return "", nil
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("pack kit for %s: %v", p.Manifest.Name, err)
	}
	dir, err := os.MkdirTemp(parent, filepath.Base(base)+kitLaunchInfix)
	if err != nil {
		return "", fmt.Errorf("pack kit for %s: %v", p.Manifest.Name, err)
	}
	fail := func(format string, a ...any) (string, error) {
		_ = os.RemoveAll(dir) // never leave a half-built kit dir behind
		return "", fmt.Errorf(format, a...)
	}
	_ = os.Chmod(dir, 0o755) // MkdirTemp creates 0700; the kit is a mounted tree
	// A stacked kit needs a valid manifest: schemaVersion (required by the loader),
	// kind: mixin, and a name. Match the base kit's schemaVersion "2".
	spec := fmt.Sprintf("schemaVersion: \"2\"\nkind: mixin\nname: %s\n", p.Manifest.Name)
	// Fold each sandbox proxy's declared egress into caps.network.allow so the
	// wrapper can actually reach its host endpoint — the sbx egress proxy blocks
	// (403) any destination not on the allowlist, even host.docker.internal. Kit
	// stacking unions this with the base kit's allowlist.
	var egress []string
	egSeen := map[string]bool{}
	addEgress := func(e string) {
		if e == "" || egSeen[e] {
			return
		}
		egSeen[e] = true
		egress = append(egress, e)
	}
	for _, pr := range sandboxProxies {
		for _, e := range pr.Egress {
			e = strings.TrimSpace(e)
			addEgress(e)
			// The sbx egress proxy matches host.docker.internal and localhost as
			// DISTINCT rules (it resolves the former to the latter), so a
			// host-loopback egress must allow BOTH forms — mirrors the base kit,
			// which lists host.docker.internal:PORT and localhost:PORT together.
			if h := strings.TrimPrefix(e, "host.docker.internal:"); h != e {
				addEgress("localhost:" + h)
			} else if l := strings.TrimPrefix(e, "localhost:"); l != e {
				addEgress("host.docker.internal:" + l)
			}
		}
	}
	if len(egress) > 0 {
		spec += "caps:\n  network:\n    allow:\n"
		for _, e := range egress {
			spec += "      - " + e + "\n"
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte(spec), 0o644); err != nil {
		return fail("pack kit for %s: %v", p.Manifest.Name, err)
	}
	if len(sandboxProxies) > 0 {
		// Proxy wrappers go under files/home/.local/bin (→ ~/.local/bin, on PATH):
		// the sbx runtime mixin-kit mount honors files/home/** (into $HOME) but NOT
		// files/usr/local/**, so a wrapper written to usr/local/bin never lands.
		binOut := filepath.Join(dir, "files", "home", ".local", "bin")
		if err := os.MkdirAll(binOut, 0o755); err != nil {
			return fail("pack kit for %s: %v", p.Manifest.Name, err)
		}
		for _, pr := range sandboxProxies {
			src := filepath.Join(p.Root, "bin", pr.Name)
			b, err := os.ReadFile(src)
			if err != nil {
				// Fail closed: never launch with a partial kit because one declared
				// wrapper couldn't be read.
				return fail("pack proxy %q: %v (refusing to build the pack kit)", pr.Name, err)
			}
			if err := os.WriteFile(filepath.Join(binOut, pr.Name), b, 0o755); err != nil {
				return fail("pack proxy %q: %v (refusing to build the pack kit)", pr.Name, err)
			}
		}
	}
	// A pack's capabilities.json travels into ~/.pi/agent so its capability
	// routing overrides the base image's generic one (what the private overlay
	// kit used to do). Fail closed if it's declared but unreadable.
	if p.CapabilitiesFile != "" {
		agentOut := filepath.Join(dir, "files", "home", ".pi", "agent")
		if err := os.MkdirAll(agentOut, 0o755); err != nil {
			return fail("pack kit for %s: %v", p.Manifest.Name, err)
		}
		b, err := os.ReadFile(p.CapabilitiesFile)
		if err != nil {
			return fail("pack capabilities.json: %v (refusing to build the pack kit)", err)
		}
		if err := os.WriteFile(filepath.Join(agentOut, "capabilities.json"), b, 0o644); err != nil {
			return fail("pack capabilities.json: %v (refusing to build the pack kit)", err)
		}
	}
	return dir, nil
}

// kitLaunchInfix names each per-launch unique kit dir as a suffix on the pack
// hash (so launch dirs sit beside their key under pack-kits/ and
// sweepStaleKitTemps finds them by prefix). kitTmpInfix/kitOldInfix are the
// LEGACY names the old swap-in-place synth used; they remain only so the sweep
// still cleans debris left by older builds.
const (
	kitLaunchInfix = ".kit-"
	kitTmpInfix    = ".tmp-"
	kitOldInfix    = ".old-"
)

// sweepStaleKitTemps best-effort removes old per-launch kit dirs (and legacy
// temp/aside/stable-path debris) for THIS pack. Only entries older than an
// hour are touched, so a concurrent launch's freshly-built kit — which sbx may
// still be reading at create time — is never yanked out from under it.
func sweepStaleKitTemps(parent, base string) {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-time.Hour)
	for _, e := range entries {
		name := e.Name()
		// base+"." covers kitLaunchInfix, kitTmpInfix, and kitOldInfix; the bare
		// base is the legacy stable kit path older builds synthesized into.
		if name != base && !strings.HasPrefix(name, base+".") {
			continue
		}
		if info, ierr := e.Info(); ierr != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.RemoveAll(filepath.Join(parent, name))
	}
}

// proxyShimTemplate is the scaffold `pack add proxy <name>` writes to bin/<name>
// (0755): a bash shim the pack author fills in.
func proxyShimTemplate(name string) string {
	return "#!/usr/bin/env bash\n" +
		"# " + name + " — pack proxy wrapper (scaffolded by `pi-stack pack add proxy`).\n" +
		"#\n" +
		"# Runs IN THE SANDBOX, fenced by the net allowlist (F2: in-sandbox, safe by\n" +
		"# default). Edit this to wrap the real CLI/API call — e.g. curl a REST\n" +
		"# endpoint, or exec a real binary already on PATH under a different name.\n" +
		"# Declare any domains it needs in pack.toml's [[proxy]] egress = [...] so\n" +
		"# the sbx kit allowlist can be updated to match.\n" +
		"set -euo pipefail\n" +
		"echo \"" + name + ": TODO — implement this wrapper\" >&2\n" +
		"exit 1\n"
}

// packLock is <pack-root>/pack.lock: GENERATED activation provenance, not a
// resolver lockfile (packs-v2-impl.md §3/ADR-1). It records exactly what the
// LAST `pack use` of this pack contributed to cfg.MCP / cfg.KnowledgeBundles /
// cfg.GogAccount / cfg.OllamaBridgeModel, so switching AWAY removes/restores
// exactly that contribution — never a user's own manually-added entry, and
// never leaking one pack's config into the next (finding #5). Git-ignored by
// default (runPackNew seeds a pack-local .gitignore line for it).
//
// TRUST (round-2 A): pack.lock is a LOCAL, HUMAN-READABLE HINT only. It sits
// inside the pack directory — attacker-writable for any cloned pack via a
// plain `git pull` (or a zip update), even for a pack that is ALREADY active
// — so NOTHING that drives a config mutation is ever read from it. The
// authoritative activation provenance lives in the launcher-owned trust
// store (packtruststore.go, Activation), written at the same commit point.
// The only field ever read back from pack.lock is Remote/Commit, used purely
// as a FAIL-SAFE adoption marker (a forged marker only RESTRICTS what a pack
// may do — isAdoptedPack).
type packLock struct {
	MCP       []string `toml:"mcp,omitempty"`
	Knowledge []string `toml:"knowledge,omitempty"`
	// Remote/Commit are set ONLY when this pack was adopted via `pack use
	// <git-url>` (clonePack) — a non-empty Remote is the provenance marker
	// isAdoptedPack reads (finding #1, CRITICAL). Once set they are preserved
	// across every later re-activation of this same pack (never re-derived from
	// a local-path `pack use`), so adoption status can't be laundered by
	// pointing `pack use` at the already-cloned local directory.
	Remote string `toml:"remote,omitempty"`
	Commit string `toml:"commit,omitempty"`
	// GogAccount/OllamaBridgeModel record the value THIS pack's last activation
	// set on cfg (only present when the pack's manifest declares the field).
	// Prior* is whatever cfg held immediately BEFORE this pack overwrote it, so
	// reverting on switch-away restores exactly that (or empty, if there was
	// none) instead of leaking this pack's value into the next one (finding #5).
	GogAccount             string `toml:"gog_account,omitempty"`
	PriorGogAccount        string `toml:"prior_gog_account,omitempty"`
	OllamaBridgeModel      string `toml:"ollama_bridge_model,omitempty"`
	PriorOllamaBridgeModel string `toml:"prior_ollama_bridge_model,omitempty"`
	// NOTHING security-relevant lives here. Trust acceptance and installed
	// host-wrapper attribution used to (Accepted*/HostWrappers) — but pack.lock
	// sits INSIDE the pack directory, i.e. inside the attacker-controlled
	// payload for any downloaded/unzipped pack, so a pre-filled lock could
	// pre-accept its own host-exec surface. Both moved to the launcher-owned
	// trust store (<config-dir>/pack-trust.json — packtruststore.go). pack.lock
	// is ONLY Phase-1 activation provenance for reversibility, and it is
	// scrubbed/ignored whenever `pack use` targets a pack that is not already
	// active (scrubUntrustedPackLock).
}

const packLockName = "pack.lock"

func packLockPath(root string) string { return filepath.Join(root, packLockName) }

// readPackLock reads root's pack.lock, best-effort: an absent OR UNPARSABLE
// file returns the zero value (no recorded contribution — the caller's removal
// set is then empty, which is the safe default: never guess at what an older
// activation contributed). A corrupt file is deliberately NOT trusted for a
// partial decode (finding #3): toml.Unmarshal can populate some fields before
// hitting a parse error, and treating that partial result as authoritative
// could silently under- or over-report a removal set. On any parse error this
// returns a clean zero value instead of whatever the decoder half-filled.
func readPackLock(root string) packLock {
	b, err := os.ReadFile(packLockPath(root))
	if err != nil {
		return packLock{}
	}
	var l packLock
	if err := toml.Unmarshal(b, &l); err != nil {
		return packLock{}
	}
	return l
}

// writePackLock writes root's pack.lock (0644; not a secret — it holds server
// NAMES and canonical bundle PATHS, never a credential value).
//
// Hardened two ways (round-3 S1 CRITICAL + R1):
//   - Lstat-REFUSES a symlinked destination. os.WriteFile FOLLOWS a symlink,
//     so a malicious cloned pack committing pack.lock as a symlink (-> /dev/null
//     or a host file) could both swallow the adoption marker (the pack then
//     reads as AUTHORED, bypassing the local-path knowledge guard) and
//     overwrite an arbitrary host file. clonePack scrubs any checked-in
//     pack.lock right after clone, so a symlink here is always hostile or
//     corrupt — never legitimate local state.
//   - Writes ATOMICALLY via a same-dir temp file + rename: an interrupted
//     write can never truncate/corrupt an existing lock, and rename REPLACES a
//     symlink rather than following it (a second layer under the Lstat check).
func writePackLock(root string, l packLock) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(l); err != nil {
		return err
	}
	return writePackLockBytes(root, buf.Bytes())
}

// writePackLockBytes is the raw-bytes half of writePackLock (same Lstat
// symlink refusal, same atomic same-dir temp + rename). Split out so
// commitPackActivation can restore a SNAPSHOT of the prior lock byte-for-byte
// on a cfg.Save failure without round-tripping it through the decoder (which
// would silently normalize — or, for an unparsable lock, erase — it).
func writePackLockBytes(root string, data []byte) error {
	dest := packLockPath(root)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	return atomicWriteInDir(root, packLockName, data, 0o644)
}

// commitPackActivation is the commit point shared by `pack use` and the
// active-pack `pack add mcp` path. It writes THREE things, in an order whose
// failure residue is always safe:
//
//  1. pack.lock (the local HINT — round-4 abort-on-unwritable contract kept:
//     a lock that cannot be written, e.g. a dir-shaped pack.lock, ABORTS
//     before anything commits).
//  2. The AUTHORITATIVE activation record in the launcher-owned trust store
//     (round-2 A) — a store-write failure also aborts before cfg.Save, so
//     the config is never committed without host-state attribution.
//  3. cfg.Save. An ordinary Save failure ROLLS BACK both the store record
//     and the prior pack.lock bytes, so on-disk state stays mutually
//     consistent (the rollback contract the round-4 tests pin, now anchored
//     on the HOST-STATE record).
//
// A true hard kill between the store write and the config rename leaves an
// activation record that OVER-claims — harmless, because removing an absent
// MCP/bundle is a no-op (see the commit-ordering comment in runPackUse).
//
// CONCURRENCY (round-3 #1): the store write, cfg.Save, and the rollback all
// run under the cross-process trust lock against a FRESH load of the store —
// saving the caller's (possibly stale) in-memory object here was the
// last-writer-wins clobber: a concurrent `pi-stack host` wrapper refresh
// could overwrite the activation this commit just wrote (or vice versa). The
// caller's store view is synced on success.
func commitPackActivation(cfg *config.Config, store *packTrustStore, root string, lock packLock) error {
	priorLock, priorErr := os.ReadFile(packLockPath(root))
	priorExists := priorErr == nil
	if priorErr != nil && !os.IsNotExist(priorErr) {
		// Can't snapshot the prior lock, so a Save-failure rollback would be
		// impossible: abort BEFORE writing anything (nothing is committed).
		return fmt.Errorf("reading prior pack.lock for %s: %v — aborting without saving config (nothing was committed; fix the pack directory and re-run)", root, priorErr)
	}
	if err := writePackLock(root, lock); err != nil {
		return fmt.Errorf("writing pack.lock for %s: %v — aborting without saving config (nothing was committed; fix the pack directory and re-run)", root, err)
	}
	restoreLock := func() error {
		if priorExists {
			return writePackLockBytes(root, priorLock)
		}
		return os.Remove(packLockPath(root))
	}
	return withPackTrustLock(func() error {
		fresh, lerr := loadPackTrustStore()
		if lerr != nil {
			if rerr := restoreLock(); rerr != nil {
				return fmt.Errorf("pack trust state unreadable: %v (and restoring the prior pack.lock failed: %v) — aborting without saving config (nothing was committed)", lerr, rerr)
			}
			return fmt.Errorf("pack trust state unreadable: %v — aborting without saving config (nothing was committed; fix %s and re-run)", lerr, packTrustStorePath())
		}
		priorActivation := fresh.Activation
		fresh.setActivation(root, lock)
		if err := fresh.save(); err != nil {
			if rerr := restoreLock(); rerr != nil {
				return fmt.Errorf("recording activation in pack trust state: %v (and restoring the prior pack.lock failed: %v) — aborting without saving config (nothing was committed)", err, rerr)
			}
			return fmt.Errorf("recording activation in pack trust state: %v — aborting without saving config (nothing was committed; fix %s and re-run)", err, packTrustStorePath())
		}
		if err := cfg.Save(); err != nil {
			// Roll BOTH the store record and the lock back so they match the
			// (unchanged) on-disk config.
			fresh.Activation = priorActivation
			serr := fresh.save()
			rerr := restoreLock()
			if serr != nil || rerr != nil {
				return fmt.Errorf("saving config: %v (rollback incomplete — trust store: %v, pack.lock: %v — the activation record may over-claim this activation's contributions; harmless, but re-run `pack use` once the config is writable)", err, serr, rerr)
			}
			return fmt.Errorf("saving config: %v (activation record rolled back; nothing was committed)", err)
		}
		if store != nil {
			store.Activation = fresh.Activation // keep the caller's view coherent
		}
		return nil
	})
}

// migratePhase1Activation is the ONE-TIME Phase-1 → Phase-2 migration
// (round-3 #2). A pack activated by a Phase-1 build recorded its activation
// attribution ONLY in pack.lock; Phase 2 reads reversibility exclusively from
// the host-state store, which treats "no record" as "remove nothing" — so
// without migration the FIRST Phase-2 switch/reactivation/rm would silently
// lose the Phase-1 pack's attribution (its MCP/knowledge contributions
// over-retained forever, its gog/model overrides never restored).
//
// It fires exactly when the store has NO activation record attributed to the
// pack, and splits on adoption:
//   - LOCAL (authored) pack: pack.lock is the user's OWN Phase-1 state — safe
//     to trust ONCE — so its attribution is lifted into the store record the
//     caller's revert reads.
//   - ADOPTED pack: pack.lock is attacker-writable payload (`git pull` / zip
//     update), so it is NEVER trusted — nothing migrates, nothing reverts
//     (safe over-retention; the exact posture of a missing lock).
//
// The migrated record is IN-MEMORY only: on a completed switch the commit
// point immediately replaces it with the new pack's record, and on an abort
// (gate refusal, commit failure) the next run simply re-migrates — nothing
// needs persisting here, and never persisting means the migration can never
// clobber a concurrent writer either.
func migratePhase1Activation(store *packTrustStore, root string) {
	if store == nil || strings.TrimSpace(root) == "" || store.hasActivationFor(root) {
		return
	}
	if isAdoptedPack(root) {
		return // adopted payload lock: never trusted (revert nothing)
	}
	hint := readPackLock(root)
	if len(hint.MCP) == 0 && len(hint.Knowledge) == 0 && hint.GogAccount == "" && hint.OllamaBridgeModel == "" {
		return // no Phase-1 attribution to migrate
	}
	store.setActivation(root, hint)
}

// isAdoptedPack reports whether root carries adoption provenance, i.e. this
// pack was cloned from a remote via `pack use <git-url>` at some point. Used
// by the finding-#1 CRITICAL guard: a shared=false local-path [[knowledge]]
// reference is NEVER honored for an adopted pack, because pack.toml there is
// attacker-controlled input from the remote — honoring it would let a
// malicious pack.toml point AddKnowledgeBundle at an arbitrary host directory
// (e.g. ~/.ssh) that the sandbox can then read via the knowledge service.
//
// Three signals, ANY of which marks the pack adopted (all fail-safe: a forged
// marker only ever RESTRICTS what a pack may do): the pack.lock Remote marker
// (clonePack/markPackAdopted write it), the launcher-owned trust store's
// adoption provenance (host state — survives even a scrubbed/forged lock),
// and the clone LOCATION itself (everything under PacksDir was put there by
// clonePack, never authored by the user).
func isAdoptedPack(root string) bool {
	if strings.TrimSpace(readPackLock(root).Remote) != "" {
		return true
	}
	if store, err := loadPackTrustStore(); err == nil {
		if _, ok := store.Adopted[canonicalizePackRoot(root)]; ok {
			return true
		}
	}
	return packRootInPacksDir(root)
}

// packRootInPacksDir reports whether root lives under config.PacksDir() — the
// directory only clonePack ever populates, so location alone proves adoption.
func packRootInPacksDir(root string) bool {
	packs := canonicalizePackRoot(config.PacksDir())
	r := canonicalizePackRoot(root)
	return packs != "" && strings.HasPrefix(r, packs+string(filepath.Separator))
}

// scrubUntrustedPackLock removes a pack-supplied pack.lock before a pack that
// is NOT currently active is adopted (item 4 of the trust-model rework): a
// downloaded/unzipped pack can ship a forged pack.lock — clonePack scrubs it
// for remote clones, but a local-path adoption used to bypass that. The forged
// file could claim the user's OWN mcp/knowledge entries as the pack's
// contribution (corrupting Phase-1 reversibility: a later switch-away would
// remove them) or be a symlink that blocks/redirects the fresh lock write.
// os.Remove removes a symlink itself, never its target. The caller must also
// IGNORE the lock's decoded content (treat as zero value) — scrubbing plus a
// fresh regenerate at commit is the whole contract.
func scrubUntrustedPackLock(root string) error {
	path := packLockPath(root)
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.IsDir() {
		// A DIRECTORY named pack.lock cannot carry forged lock content
		// (readPackLock zero-values it) — leave it for the commit point, which
		// fails loudly with the established abort-without-commit message.
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing untrusted %s: %w", packLockName, err)
	}
	return nil
}

// errPrivateRefSkippedAdopted is the sentinel resolvePackKnowledgeRef returns
// when it refuses to honor a shared=false local-path reference because the
// pack is adopted (finding #1). Callers use errors.Is to distinguish this from
// an ordinary resolution failure so they can batch it into one aggregate
// notice instead of per-ref noise.
var errPrivateRefSkippedAdopted = errors.New("private knowledge ref skipped: pack is adopted from a remote")

// revertPackPriorContribution undoes a previous activation's contribution
// (F4/finding #5): removes exactly the MCP + knowledge entries prevLock
// attributes to that pack (never a value the lock doesn't mention — the
// finding #3 reversibility guarantee), and restores gog_account /
// ollama_bridge_model to whatever cfg held immediately before that pack
// overwrote them (or empty, if there was none). Shared by runPackUse
// (switching to a different pack, or re-activating the SAME pack — finding D)
// and runPackRm (detaching), so all are equally honest about what
// "detached"/"switched away" means.
func revertPackPriorContribution(cfg *config.Config, prevLock packLock) (removedMCP, removedKnowledge []string) {
	for _, m := range prevLock.MCP {
		if cfg.RemoveMCP(m) {
			removedMCP = append(removedMCP, m)
		}
	}
	for _, id := range prevPackKnowledgeIDs(prevLock) {
		if cfg.RemoveKnowledgeBundle(id) {
			removedKnowledge = append(removedKnowledge, id)
		}
	}
	// Only revert if cfg still holds exactly what THIS pack set — never clobber
	// a value something else changed in the meantime.
	if prevLock.GogAccount != "" && cfg.GogAccount == prevLock.GogAccount {
		cfg.SetGogAccount(prevLock.PriorGogAccount)
	}
	if prevLock.OllamaBridgeModel != "" && cfg.OllamaBridgeModel == prevLock.OllamaBridgeModel {
		cfg.OllamaBridgeModel = prevLock.PriorOllamaBridgeModel
	}
	return removedMCP, removedKnowledge
}

// canonicalizePackRoot normalizes a pack root path for identity comparison
// (finding #7): expands ~, then filepath.Abs + Clean, so e.g. `pack add mcp
// fastmail ./work` compares correctly against cfg.Pack even when cfg.Pack is
// stored absolute and the CLI argument is a relative path (or vice versa).
// Best-effort: a path that can't be made absolute falls back to
// expandUser+Clean rather than failing.
func canonicalizePackRoot(p string) string {
	p = expandUser(strings.TrimSpace(p))
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(p)
}

// prevPackKnowledgeIDs computes the canonical bundle ids the PREVIOUS active
// pack contributed, for removal on switch (F4). STRICTLY lock-attributed
// (finding C): only entries pack.lock records as that activation's own
// contribution are ever removed. An empty/missing/corrupt lock removes
// NOTHING — possible stale-bundle accumulation is accepted over the
// alternative of guessing from the manifest, which could delete a bundle the
// USER added independently (the old embedded-knowledge/ fallback did exactly
// that when the lock was lost).
func prevPackKnowledgeIDs(lock packLock) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range lock.Knowledge {
		id = canonicalizeKnowledgeBundle(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// resolvePackKnowledgeRef resolves one [[knowledge]] entry to an absolute local
// bundle path (F6). The guards here key on the source's CLASS (git URL vs
// local path — knowledgeSourceIsGitURL), NEVER on the manifest's shared flag,
// which an attacker authors (finding A, CRITICAL): keying the skip-guard on
// shared=false alone let an adopted pack declare shared=true with a LOCAL path
// and walk straight past it into AddKnowledgeBundle. loadPack additionally
// enforces shared↔class agreement (shared=true ⇔ git URL), so a mismatched
// entry never even loads; the class check here keeps this function safe for
// any caller regardless.
//
// A git URL TRAVELS: resolved via resolveBundleRef, which clones/pulls it into
// the shared knowledge cache through the safeGitURL gate (no ext::/fd::/file::
// transports, no local-as-remote) — an adopter who shares the pack pulls the
// SAME team bundle.
//
// A LOCAL path is AUTHORED-ONLY: it is deliberately NOT root-scoped (pointing
// outside the pack at the owner's own machine is the entire point of a private
// reference), but pack.toml for an ADOPTED pack (cloned from a remote,
// adopted==true) is attacker-controlled input, so a local path there is NEVER
// honored — whatever the shared flag claims — or `pack use <attacker-git-url>`
// could point AddKnowledgeBundle at an arbitrary host directory (e.g. ~/.ssh)
// that the knowledge service then indexes and the sandbox can read. The caller
// aggregates these into one notice (see errPrivateRefSkippedAdopted).
//
// For a pack the user authored locally (adopted==false), two more guards apply
// before AddKnowledgeBundle ever sees the path: (a) it must resolve to an
// existing, readable directory — a typo'd/nonexistent path is skipped rather
// than indexed, so the knowledge service is never pointed at a dangling entry
// (no knowledge-service poisoning); (b) it must resolve OUTSIDE the pack's own
// tree — a private reference that actually lives inside root should be embedded
// under knowledge/ instead (that travels honestly), not declared "private" while
// silently living in the repo.
func resolvePackKnowledgeRef(out io.Writer, root string, adopted bool, k packKnowledge) (string, error) {
	source := strings.TrimSpace(k.Source)
	if source == "" {
		return "", fmt.Errorf("[[knowledge]] %q has no source", k.Name)
	}
	if knowledgeSourceIsGitURL(source) {
		return resolveBundleRef(source, knowledgeCacheDir(), out)
	}
	// LOCAL path: authored-only, regardless of the shared flag (finding A).
	if adopted {
		return "", errPrivateRefSkippedAdopted
	}
	abs, err := filepath.Abs(expandUser(source))
	if err != nil {
		return "", fmt.Errorf("resolving private knowledge %q: %w", k.Name, err)
	}
	resolved := filepath.Clean(abs)
	if r, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		resolved = r
	}
	// (b) reject a source resolving inside the pack tree.
	rootResolved := filepath.Clean(root)
	if r, rerr := filepath.EvalSymlinks(root); rerr == nil {
		rootResolved = r
	}
	if resolved == rootResolved || strings.HasPrefix(resolved, rootResolved+string(filepath.Separator)) {
		return "", fmt.Errorf("private knowledge %q (%s) resolves INSIDE the pack tree; embed it under knowledge/ instead of referencing it as private", k.Name, source)
	}
	// (a) validate it exists and is a readable directory before AddKnowledgeBundle.
	fi, statErr := os.Stat(resolved)
	if statErr != nil {
		return "", fmt.Errorf("private knowledge %q: %s: %w", k.Name, resolved, statErr)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("private knowledge %q: %s is not a directory", k.Name, resolved)
	}
	f, openErr := os.Open(resolved)
	if openErr != nil {
		return "", fmt.Errorf("private knowledge %q: %s is not readable: %w", k.Name, resolved, openErr)
	}
	f.Close()
	return resolved, nil
}

// writeMemoryScope writes (or removes) <workspace>/.pi-stack/profile: the
// memory scope tag the in-VM recall/capture extensions already read
// (memory-recall.ts, memory-capture.ts — no extension change for F4). p is the
// active pack (nil when none). The scope is p.Manifest.MemoryScope, defaulting
// to the pack's Name; an empty result or the literal "default" selects the
// shared/unscoped tag, matching "default" == the shared scope from the schema
// doc. No pack (or an unscoped pack) removes any stale file — this REPLACES the
// old unconditional profile-delete in run.go. Symlink-safe via
// writeWorkspaceStateFile (a hostile repo can commit .pi-stack/profile as a
// symlink) and removeWorkspaceStateFile (a hostile repo can commit .pi-stack
// ITSELF as a symlink to another repo's .pi-stack, which a plain os.Remove
// would traverse and delete through).
func writeMemoryScope(workspace string, p *packInfo) {
	if p == nil {
		_ = removeWorkspaceStateFile(workspace, "profile")
		return
	}
	// Memory is a single SHARED store by default (AGENTS: the in-store scope column
	// is dormant). ONLY an explicit `memory_scope` in the manifest isolates a pack.
	// The pack NAME must NOT become a scope: doing so stamped every conversational
	// capture with the pack's own name, which hid it from the default recall view
	// (recall sees {scope}∪{default}; host-side recall queries default), so
	// captured preferences looked lost. Empty/"default" => shared, no scope file.
	// (The default pack's own Name IS literally "default", so this guard is what
	// keeps its captures shared rather than accidentally scoped to itself.)
	scope := strings.TrimSpace(p.Manifest.MemoryScope)
	if scope == "" || scope == "default" {
		_ = removeWorkspaceStateFile(workspace, "profile")
		return
	}
	_ = writeWorkspaceStateFile(workspace, "profile", []byte(scope+"\n"), 0o644)
}

// writePackContextFiles writes the two per-launch, pack-scoped workspace files
// that carry the active pack's context into a sandbox: the ollama-bridge model
// (.pi-stack/ollama-bridge.model, via writeOllamaBridgeFile) and the memory
// scope (.pi-stack/profile, via writeMemoryScope, resolved from the active
// pack). Shared by `pi-stack run` and `pi-stack task new` so a task sandbox
// gets the SAME pack context as a normal run — packs-v2 Phase 1 had `run`
// write these but not `task new`. Best-effort throughout: an unloadable pack
// degrades to unscoped memory rather than failing the launch (mirrors
// writeMemoryScope's own contract).
//
// effectivePack is the root applyPackToLaunch actually applied (its returned
// value) — NOT activePackRoot(cfg.Pack, o.Pack) directly. This keeps memory
// scoping honest with the sandbox.pack marker: when applyPackToLaunch
// degraded (errNotAPack), effectivePack is "" and memory stays unscoped, even
// though cfg.Pack/o.Pack still name the (unavailable) configured pack.
func writePackContextFiles(cfg *config.Config, o runOpts, effectivePack string) {
	writeOllamaBridgeFile(o.Workspace, cfg.OllamaBridgeModel)
	var activePack *packInfo
	if effectivePack != "" {
		if lp, lerr := loadPack(effectivePack); lerr == nil {
			activePack = lp
		}
	}
	writeMemoryScope(o.Workspace, activePack)
}

// packRecreateLine is the ADR-3 "same breath" recreate instruction: any
// operation that changes the sandbox facet set (MCP attach, sandbox bin/
// wrappers) MUST print this, because --mcp/--kit are create-only — a running
// sandbox cannot pick either up without a recreate (packs.md §13 must-fix).
func printPackRecreateLine(out io.Writer) {
	fmt.Fprintln(out, "MCP attach + sandbox bin/ wrappers + pack skills only take effect on a sandbox CREATE.")
	fmt.Fprintln(out, "Recreate to pick them up:  pi-stack run --replace")
}

// --- verb tree --------------------------------------------------------------

func runPackCmd(argv []string) {
	if wantsHelp(argv) {
		fmt.Print(packUsage)
		return
	}
	sub := "ls"
	var rest []string
	if len(argv) > 0 {
		sub, rest = argv[0], argv[1:]
	}
	env := defaultShellEnv()
	switch sub {
	case "new":
		runPackNew(env, os.Stdout, rest)
	case "add":
		runPackAdd(env, os.Stdout, rest)
	case "ls":
		runPackLs(os.Stdout)
	case "show":
		runPackShow(os.Stdout, rest)
	case "use":
		runPackUse(env, os.Stdout, rest)
	case "rm":
		runPackRm(os.Stdout, rest)
	default:
		fmt.Fprintf(os.Stderr, "pi-stack pack: unknown subcommand %q (want: new, add, ls, show, use, rm)\n", sub)
		os.Exit(2)
	}
}

// packTarget resolves an optional positional PATH to a pack root, defaulting to
// the default pack root.
func packTarget(rest []string) string {
	if len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		return expandUser(rest[0])
	}
	return defaultPackRoot()
}

// safeArtifactName rejects a skill/knowledge name that could escape the pack root
// (path separators, `..`) or is empty.
func safeArtifactName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// activateDefaultPack sets the default pack as active (config `pack`) if no
// pack is active yet, so implicit-create makes it immediately usable (no manual
// `pack use`). Best-effort. Only for the default pack root.
// activateDefaultPack points cfg.Pack at root when (and only when) root IS the
// resolved default pack AND cfg.Pack is currently empty — it must NEVER
// override an explicitly active alternate pack (cfg.Pack != ""), which is a
// no-op, not an error. Returns an error instead of swallowing one: a caller
// that reports "active pack -> this (default) pack" (or, worse, propagates
// setup success) after a cfg.Save failure would be lying — the config on
// disk still has no active pack. Every caller MUST check the returned error
// before claiming activation succeeded.
func activateDefaultPack(root string) error {
	if root != defaultPackRoot() {
		return nil // not the default pack; nothing for this to do
	}
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config to activate default pack: %w", err)
	}
	if cfg.Pack != "" {
		return nil // an explicitly active (possibly alternate) pack is never overridden
	}
	cfg.Pack = root
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("could not save config to activate default pack %s: %w", root, err)
	}
	return nil
}

// runPackNew adopts a pre-existing repo (or one already carrying pack.toml) in
// place, else creates + git-inits a fresh pack. Never re-inits or clobbers.
func runPackNew(env shellEnv, out io.Writer, rest []string) {
	root := packTarget(rest)
	// Already a pack? Nothing to do (but ensure the default one is active).
	if _, err := os.Stat(filepath.Join(root, packManifestName)); err == nil {
		fmt.Fprintf(out, "already a pack: %s\n", root)
		if aerr := activateDefaultPack(root); aerr != nil {
			fmt.Fprintf(out, "pi-stack pack new: %v\n", aerr)
			os.Exit(1)
		}
		return
	}
	existsDir := false
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		existsDir = true
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(out, "pi-stack pack new: %v\n", err)
		os.Exit(1)
	}
	// git init only if it isn't already a repo (adopt an existing one in place).
	isRepo := false
	if _, err := os.Stat(filepath.Join(root, ".git")); err == nil {
		isRepo = true
	}
	if !isRepo && env.run != nil {
		if _, err := env.run("git", "-C", root, "init"); err != nil {
			fmt.Fprintf(out, "  note: `git init` failed (%v) — the pack still works; init it yourself\n", err)
		} else {
			isRepo = true
		}
	}
	name := filepath.Base(root)
	if err := writePackManifest(root, packManifest{Name: name, Schema: 1}); err != nil {
		fmt.Fprintf(out, "pi-stack pack new: could not write %s: %v\n", packManifestName, err)
		os.Exit(1)
	}
	// pack.lock is GENERATED activation provenance (ADR-1), never hand-authored;
	// seed a pack-local .gitignore line for it so a fresh pack never accidentally
	// commits it. Best-effort, no-clobber (never touches an existing .gitignore
	// beyond appending the line once).
	seedPackGitignore(root)
	switch {
	case existsDir && isRepo:
		fmt.Fprintf(out, "adopted existing repo as pack %q: %s\n", name, root)
	case isRepo:
		fmt.Fprintf(out, "created pack %q (git-initialized): %s\n", name, root)
	default:
		fmt.Fprintf(out, "created pack %q: %s (git init it to version it)\n", name, root)
	}
	// Auto-activate the default pack so it is immediately usable (no manual
	// `pack use` for the common case). A named/other pack still needs `pack use`.
	if root == defaultPackRoot() {
		if aerr := activateDefaultPack(root); aerr != nil {
			fmt.Fprintf(out, "pi-stack pack new: %v\n", aerr)
			os.Exit(1)
		}
		fmt.Fprintln(out, "active pack -> this (default) pack")
	} else {
		fmt.Fprintf(out, "use it:  pi-stack pack use %s\n", root)
	}
}

// runPackAdd writes one artifact into a pack (implicit-create), then registers
// it by presence (skills/knowledge are discovered by convention).
func runPackAdd(env shellEnv, out io.Writer, rest []string) {
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pi-stack pack add <skill|knowledge|proxy|mcp> <name> [PACK] [flags]")
		os.Exit(2)
	}
	kind, name := rest[0], rest[1]
	if !safeArtifactName(name) {
		fmt.Fprintf(os.Stderr, "pi-stack pack add: invalid name %q (letters, digits, -, _, . only; no path separators)\n", name)
		os.Exit(2)
	}
	// Parse the tail: flags (--host, --private, --ref VALUE, --env VALUE) plus an
	// optional trailing PACK positional. Flags are shared across kinds; each kind
	// below reads only the ones it understands.
	var host, private, yes bool
	var ref, envVar string
	var positionals []string
	tail := rest[2:]
	for i := 0; i < len(tail); i++ {
		a := tail[i]
		switch {
		case a == "--host":
			host = true
		case a == "--private":
			private = true
		case a == "--yes" || a == "-y":
			yes = true
		case a == "--ref":
			if i+1 >= len(tail) {
				fmt.Fprintln(os.Stderr, "pi-stack pack add: --ref needs a value")
				os.Exit(2)
			}
			i++
			ref = tail[i]
		case strings.HasPrefix(a, "--ref="):
			ref = strings.TrimPrefix(a, "--ref=")
		case a == "--env":
			if i+1 >= len(tail) {
				fmt.Fprintln(os.Stderr, "pi-stack pack add: --env needs a value")
				os.Exit(2)
			}
			i++
			envVar = tail[i]
		case strings.HasPrefix(a, "--env="):
			envVar = strings.TrimPrefix(a, "--env=")
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "pi-stack pack add: unknown flag %q\n", a)
			os.Exit(2)
		default:
			positionals = append(positionals, a)
		}
	}
	root := defaultPackRoot()
	if len(positionals) >= 1 {
		root = expandUser(positionals[0])
	}
	// Implicit-create the pack if absent.
	if _, err := os.Stat(filepath.Join(root, packManifestName)); err != nil {
		runPackNew(env, out, []string{root})
	}
	// Refuse to write through a symlinked skills/knowledge/bin dir (an adopted
	// pack could point it outside the pack root); same posture as loadPack's
	// mount check.
	for _, d := range []string{"skills", "knowledge", "bin"} {
		if isSymlinkPath(filepath.Join(root, d)) {
			fmt.Fprintf(os.Stderr, "pi-stack pack add: %s has a symlinked %s/ dir; refusing to write through it\n", root, d)
			os.Exit(1)
		}
	}
	switch kind {
	case "skill":
		dir := filepath.Join(root, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		f := filepath.Join(dir, "SKILL.md")
		if _, err := os.Stat(f); err == nil {
			fmt.Fprintf(out, "skill already exists: %s\n", f)
			return
		}
		if err := os.WriteFile(f, []byte(skillTemplate(name)), 0o644); err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added skill %q: %s\n", name, f)
		fmt.Fprintln(out, "edit it, then commit it to your pack's git repo.")
	case "knowledge":
		if strings.TrimSpace(ref) == "" {
			// Embed (v1 behavior): a literal knowledge/ doc, discovered by convention.
			dir := filepath.Join(root, "knowledge")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
				os.Exit(1)
			}
			f := filepath.Join(dir, name+".md")
			if _, err := os.Stat(f); err == nil {
				fmt.Fprintf(out, "knowledge doc already exists: %s\n", f)
				return
			}
			if err := os.WriteFile(f, []byte(knowledgeTemplate(name)), 0o644); err != nil {
				fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(out, "added knowledge doc %q: %s\n", name, f)
			return
		}
		// F6 reference: [[knowledge]] name/source/shared. --private (shared=false)
		// does NOT travel with the pack — the source is a local path that stays on
		// this machine; the default (shared=true) is meant for a git URL an adopter
		// pulls.
		p, err := loadPack(root)
		if err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		entry := packKnowledge{Name: name, Source: ref, Shared: !private}
		replaced := false
		for i, k := range p.Manifest.Knowledge {
			if k.Name == name {
				p.Manifest.Knowledge[i] = entry
				replaced = true
				break
			}
		}
		if !replaced {
			p.Manifest.Knowledge = append(p.Manifest.Knowledge, entry)
		}
		if err := writePackManifest(root, p.Manifest); err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added knowledge reference %q (shared=%v) to pack.toml\n", name, entry.Shared)
		if private {
			fmt.Fprintln(out, "private: this reference will NOT travel if you share the pack.")
		}
		fmt.Fprintln(out, "run `pi-stack pack use` on this pack to index it.")
	case "proxy":
		binDir := filepath.Join(root, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		f := filepath.Join(binDir, name)
		if _, err := os.Stat(f); err != nil {
			if err := os.WriteFile(f, []byte(proxyShimTemplate(name)), 0o755); err != nil {
				fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(out, "scaffolded proxy wrapper: %s\n", f)
		} else {
			fmt.Fprintf(out, "proxy wrapper already exists: %s\n", f)
		}
		p, err := loadPack(root)
		if err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		exists := false
		for i, pr := range p.Manifest.Proxies {
			if pr.Name == name {
				p.Manifest.Proxies[i].Host = host
				exists = true
				break
			}
		}
		if !exists {
			p.Manifest.Proxies = append(p.Manifest.Proxies, packProxy{Name: name, Host: host})
		}
		if err := writePackManifest(root, p.Manifest); err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added proxy %q to pack.toml (host=%v)\n", name, host)
		if host {
			// F3: a host wrapper is a Tier-1 facet — it installs only after the
			// F5 BoM gate accepts it at `pack use`, and it is on PATH for
			// `pi-stack host` sessions ONLY (never the sandbox), behind the
			// host.enabled machine gate.
			fmt.Fprintf(out, "host wrapper: review + accept it with `pi-stack pack use %s` (Tier-1 host BoM gate);\n", root)
			fmt.Fprintln(out, "once accepted it installs for `pi-stack host` sessions only (requires host.enabled).")
		} else {
			// Edit it, then a sandbox recreate is needed to mount it (F2/ADR-3).
			printPackRecreateLine(out)
		}
	case "mcp":
		p, err := loadPack(root)
		if err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		exists := false
		for i, ig := range p.Manifest.Integrations {
			if ig.MCP == name {
				p.Manifest.Integrations[i].Env = envVar
				exists = true
				break
			}
		}
		if !exists {
			p.Manifest.Integrations = append(p.Manifest.Integrations, packIntegration{Name: name, MCP: name, Env: envVar})
		}
		if err := writePackManifest(root, p.Manifest); err != nil {
			fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(out, "added mcp integration %q to pack.toml\n", name)
		// F1: if this IS the active pack, attach it right now (cfg.MCP + gateway
		// registration + credential solicit), same mechanism as `pack use` —
		// otherwise nothing has changed in the sandbox facet set yet, so no
		// recreate line is owed until the pack is actually activated.
		cfg, cerr := config.Load()
		// finding #7: compare CANONICALIZED paths — root here may be a raw
		// (possibly relative) CLI argument, while cfg.Pack is stored resolved
		// absolute by `pack use`. A raw-string compare made an active pack look
		// inactive whenever the two spellings differed (e.g. `pack add mcp
		// fastmail ./work` while cfg.Pack is the absolute form).
		if cerr == nil && canonicalizePackRoot(cfg.Pack) == canonicalizePackRoot(root) {
			// F5: attaching an MCP means the gateway runs its command ON THE
			// HOST — a Tier-1 fact. Same gate as `pack use`: an exact
			// fingerprint match in the HOST trust store skips the prompt,
			// non-TTY fails closed unless --yes. On refusal the declaration
			// stays in pack.toml (inert) but NOTHING attaches.
			trustStore, tserr := loadPackTrustStore()
			if tserr != nil {
				// FATAL (round-2 A): the store is both the acceptance source and
				// the activation-attribution commit target; an empty stand-in
				// would clobber it at the commit point.
				fmt.Fprintf(out, "pi-stack pack add: pack trust state unreadable: %v (fix or remove %s and re-run)\n", tserr, packTrustStorePath())
				os.Exit(1)
			}
			// One-time Phase-1 → Phase-2 migration (round-3 #2): without it, a
			// Phase-1 active pack's lock attribution would be overwritten at the
			// commit below with ONLY the newly-added name.
			migratePhase1Activation(trustStore, root)
			bom := computeHostBoM(p, cfg.GogAccount, localMCPClassifier(env, hostBinaryResolver))
			var bomFingerprint, packKey string
			if bom.tier1() {
				fp, _, ferr := computeHostExecFingerprint(root, bom)
				if ferr != nil {
					fmt.Fprintf(out, "pi-stack pack add: pack %s: %v\n", root, ferr)
					os.Exit(1)
				}
				bomFingerprint = fp
				packKey = trustStore.trustKey(root)
				if got, ok := trustStore.acceptedFingerprint(packKey); !ok || got != fp {
					if gerr := packTrustGate(os.Stdin, out, isTTY(os.Stdin), yes, p.Manifest.Name, bom); gerr != nil {
						fmt.Fprintf(out, "pi-stack pack add: %v (declared in pack.toml, but NOT attached)\n", gerr)
						os.Exit(1)
					}
				}
			}
			added := cfg.AddMCP(name)
			if added {
				// Attribution stays gated on the AddMCP result (finding #2): a
				// pre-existing, user-added name is never claimed as this pack's.
				// Lock BEFORE Save, ABORT on lock failure (round-3 R1 + round-4 F1,
				// same commit point as runPackUse): the config is never committed
				// without its attribution, so a later `pack use`/`pack rm` can
				// always clean up what this command added. The attribution BASE is
				// the HOST-state activation record (round-2 A), never the payload
				// lock; the adoption marker for the hint comes from host-recorded
				// provenance first, else the (fail-safe) payload marker.
				lock := trustStore.activationFor(root)
				if !containsStr(lock.MCP, name) {
					lock.MCP = append(lock.MCP, name)
				}
				if prov, ok := trustStore.Adopted[canonicalizePackRoot(root)]; ok {
					lock.Remote, lock.Commit = prov.Remote, prov.Commit
				} else if hint := readPackLock(root); strings.TrimSpace(hint.Remote) != "" {
					lock.Remote, lock.Commit = strings.TrimSpace(hint.Remote), strings.TrimSpace(hint.Commit)
				}
				if err := commitPackActivation(cfg, trustStore, root, lock); err != nil {
					fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
					os.Exit(1)
				}
			}
			// F5: persist the acceptance in HOST state (the gate above passed,
			// or the stored fingerprint already covered this surface), so a
			// later `pack use` of this pack won't re-prompt for what was just
			// accepted here. Best-effort: a failed write only re-prompts.
			if bom.tier1() {
				// Lock-serialized fresh-load mutation (round-3 #1); commit is
				// provenance metadata only (round-3 #5).
				rec := packTrustRecord{Path: canonicalizePackRoot(root), Fingerprint: bomFingerprint}
				if _, werr := mutatePackTrustStore(func(s *packTrustStore) error {
					if prov, ok := s.Adopted[rec.Path]; ok {
						rec.Remote, rec.Commit = prov.Remote, prov.Commit
					}
					s.recordAcceptance(packKey, rec)
					return nil
				}); werr != nil {
					fmt.Fprintf(out, "note: could not record the accepted host BoM: %v (the Tier-1 gate will re-prompt)\n", werr)
				}
			}
			// finding E: registration runs even when the name was ALREADY in
			// cfg.MCP (added == false) — it is idempotent, and a retry after a
			// failed gateway registration must actually re-register instead of
			// silently doing nothing.
			if err := registerServers(cfg, env, out, []string{name}, findHostBinary, packContainerMCP(p)); err != nil {
				fmt.Fprintf(out, "note: mcp registration: %v\n", err)
			}
			solicitPackCredentials(env, os.Stdin, out, isTTY(os.Stdin), p)
			if added {
				printPackRecreateLine(out)
			}
		} else {
			fmt.Fprintf(out, "activate the pack to attach it to a sandbox:  pi-stack pack use %s\n", root)
		}
	default:
		fmt.Fprintf(os.Stderr, "pi-stack pack add: unknown kind %q (want: skill, knowledge, proxy, mcp)\n", kind)
		os.Exit(2)
	}
}

func runPackLs(out io.Writer) {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pi-stack pack ls: %v\n", err)
		os.Exit(1)
	}
	active := activePackRoot(cfg.Pack, "")
	if active == "" {
		fmt.Fprintln(out, "no active pack (`pi-stack pack add skill <name>` to start one, or `pi-stack pack use <path|git-url>`)")
		return
	}
	p, err := loadPack(active)
	if err != nil {
		fmt.Fprintf(out, "pack %s: %v\n", active, err)
		return
	}
	fmt.Fprintf(out, "active pack: %s (%s)\n", p.Manifest.Name, p.Root)
}

func runPackShow(out io.Writer, rest []string) {
	root := packTarget(rest)
	if len(rest) == 0 {
		cfg, err := config.Load()
		if err == nil && activePackRoot(cfg.Pack, "") != "" {
			root = activePackRoot(cfg.Pack, "")
		}
	}
	p, err := loadPack(root)
	if err != nil {
		fmt.Fprintf(out, "pi-stack pack show: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "pack:      %s\n", p.Manifest.Name)
	fmt.Fprintf(out, "root:      %s\n", p.Root)
	fmt.Fprintf(out, "skills:    %s\n", present(p.SkillsDir))
	fmt.Fprintf(out, "knowledge: %s\n", present(p.KnowledgeDir))
	if p.CapabilitiesFile != "" {
		fmt.Fprintln(out, "capabilities: yes (mounts to ~/.pi/agent/capabilities.json)")
	}
	if p.Manifest.OllamaBridgeModel != "" {
		fmt.Fprintf(out, "ollama:    %s\n", p.Manifest.OllamaBridgeModel)
	}
	if p.Manifest.GogAccount != "" {
		fmt.Fprintf(out, "gog:       %s\n", p.Manifest.GogAccount)
	}
	if p.Manifest.MemoryScope != "" {
		fmt.Fprintf(out, "memory:    %s\n", p.Manifest.MemoryScope)
	}
	if len(p.Manifest.Proxies) > 0 {
		fmt.Fprintln(out, "proxies:")
		for _, pr := range p.Manifest.Proxies {
			kind := "sandbox bin/"
			if pr.Host {
				kind = "HOST (Phase 2)"
			}
			fmt.Fprintf(out, "  - %s (%s)\n", pr.Name, kind)
		}
	}
	if len(p.Manifest.Knowledge) > 0 {
		fmt.Fprintln(out, "knowledge refs:")
		for _, k := range p.Manifest.Knowledge {
			shared := "private (does not travel)"
			if k.Shared {
				shared = "shared (travels)"
			}
			fmt.Fprintf(out, "  - %s -> %s [%s]\n", k.Name, k.Source, shared)
		}
	}
	if len(p.Manifest.Integrations) > 0 {
		env := defaultShellEnv()
		fmt.Fprintln(out, "integrations:")
		for _, ig := range p.Manifest.Integrations {
			fmt.Fprintf(out, "  - %s", ig.Name)
			if ig.MCP != "" {
				fmt.Fprintf(out, " (mcp: %s)", ig.MCP)
			}
			switch {
			case ig.Manifest != "":
				// Manifest container: creds are Docker-side, not op-refs.
				fmt.Fprintf(out, " — manifest: %s (creds Docker-side)", ig.Manifest)
			case ig.Image != "":
				fmt.Fprintf(out, " — image: %s", ig.Image)
			case ig.URL != "":
				// Remote endpoint: OAuth'd host-side by the gateway, no op-refs.
				fmt.Fprintf(out, " — url: %s (OAuth host-side)", ig.URL)
			}
			// Image + host/remote integrations resolve their secret from op-refs
			// (Manifest containers don't — those are Docker-side).
			if ig.Env != "" && ig.Manifest == "" {
				if opRefFilled(env, ig.Env) {
					fmt.Fprintf(out, " — %s ✓", ig.Env)
				} else {
					fmt.Fprintf(out, " — %s ✗ (run: pi-stack secret set %s op://vault/item/field)", ig.Env, ig.Env)
				}
			}
			fmt.Fprintln(out)
		}
	}
}

// opRefFilled reports whether op-refs.env has a FILLED op:// ref for env var key.
func opRefFilled(env shellEnv, key string) bool {
	_, content, exists := opRefsContent(env)
	if !exists {
		return false
	}
	for _, r := range parseOpRefs(content) {
		if r.key == key && r.isRef && !r.placeholder {
			return true
		}
	}
	return false
}

// solicitPackCredentials, on a TTY, prompts for any pack integration whose op://
// credential ref is missing, writing each accepted ref. No-op off-TTY or when op
// isn't installed; missing refs then just surface as warnings at run time. The
// pack ships no secret — only the user's own op:// reference is stored.
func solicitPackCredentials(env shellEnv, in io.Reader, out io.Writer, tty bool, p *packInfo) {
	if !tty || in == nil || !opInstalled(env) {
		return
	}
	var missing []packIntegration
	for _, ig := range p.Manifest.Integrations {
		if ig.Env == "" {
			continue
		}
		if !envVarNameRe.MatchString(ig.Env) {
			fmt.Fprintf(out, "  (skipping integration %q: invalid env var name %q)\n", ig.Name, ig.Env)
			continue
		}
		if !opRefFilled(env, ig.Env) {
			missing = append(missing, ig)
		}
	}
	if len(missing) == 0 {
		return
	}
	fmt.Fprintf(out, "\nThis pack uses %d integration(s) needing a 1Password credential.\n", len(missing))
	sc := bufio.NewScanner(in)
	for _, ig := range missing {
		fmt.Fprintf(out, "  %s -> op:// ref for %s (Enter to skip): ", ig.Name, ig.Env)
		if !sc.Scan() {
			return
		}
		ref := normalizeOpRef(sc.Text())
		if ref == "" {
			continue
		}
		if !strings.HasPrefix(ref, "op://") {
			fmt.Fprintf(out, "    skipped %s: not an op:// ref\n", ig.Env)
			continue
		}
		if err := writeOpRefQuiet(env, ig.Env, ref); err != nil {
			fmt.Fprintf(out, "    could not save %s: %v\n", ig.Env, err)
			continue
		}
		fmt.Fprintf(out, "    saved %s\n", ig.Env)
	}
}

func runPackUse(env shellEnv, out io.Writer, rest []string) {
	// --yes / -y accepts the F5 Tier-1 host BoM without prompting (the ONLY way
	// a non-TTY adoption of a host-exec pack can proceed — it fails closed
	// otherwise).
	var yes bool
	var args []string
	for _, a := range rest {
		switch a {
		case "--yes", "-y":
			yes = true
		default:
			args = append(args, a)
		}
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: pi-stack pack use [--yes] <path|git-url|default>")
		os.Exit(2)
	}
	arg := strings.TrimSpace(args[0])
	// "default" is a real built-in alias for the default pack root (NOT
	// $PWD/default): resolves through defaultPackRoot() exactly like every other
	// call site, running the same legacy migration. "personal" is a DEPRECATED
	// alias kept for backward compatibility with a deprecation warning; only the
	// EXACT bare token matches — a git URL or a real path/dir literally named
	// "personal" (e.g. `./personal`, `../personal`, a full path, or a git URL
	// ending in personal.git) is unaffected and still resolves as a path/URL.
	switch arg {
	case "default":
		arg = defaultPackRoot()
	case "personal":
		fmt.Fprintln(out, "pi-stack pack use: \"personal\" is deprecated; use \"default\" instead.")
		arg = defaultPackRoot()
	}
	var root, remoteURL, remoteCommit string
	if isPackGitURL(arg) {
		r, err := clonePack(env, out, arg)
		if err != nil {
			fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
			os.Exit(1)
		}
		root = r
		remoteURL, _ = parsePackURL(arg)
		if env.run != nil {
			if sha, cerr := env.run("git", "-C", root, "rev-parse", "HEAD"); cerr == nil {
				remoteCommit = strings.TrimSpace(sha)
			}
		}
	} else {
		root = expandUser(arg)
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
	}
	p, err := loadPack(root)
	if err != nil {
		fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
		os.Exit(1)
	}
	// F5: re-hash every SHA-pinned [[bin]] BEFORE anything commits or the gate
	// even renders — activating a pack whose pinned binary does not match its
	// declared sha is refused outright (tampered binary or stale pin), so the
	// sha the BoM screen shows is always the sha of the actual bytes on disk.
	for _, bn := range p.Manifest.Bins {
		if verr := verifyPackBinSHA(root, bn); verr != nil {
			fmt.Fprintf(out, "pi-stack pack use: pack %s: %v\n", root, verr)
			os.Exit(1)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
		os.Exit(1)
	}

	// --- F4: the atomic swap. Everything below mutates the in-memory cfg; the
	// ONE cfg.Save() further down is the single commit point for every host-side/
	// config facet (ADR-3). Nothing is half-written to config if Save fails: the
	// pre-Save cfg was never persisted. ---

	prevRoot := cfg.Pack
	switching := prevRoot != "" && prevRoot != root
	// The pack-supplied pack.lock is NEVER trusted for reversibility (round-2
	// A) — not even when this pack is already active: a plain `git pull` (or a
	// zip update) rewrites files under an already-active pack root, so a forged
	// lock claiming the user's own mcp/knowledge entries as the pack's
	// contribution would make the revert below DELETE them. The ONLY thing
	// read off the lock is the fail-safe adoption marker (Remote/Commit — a
	// forged marker only RESTRICTS what a pack may do). Reversibility reads
	// come exclusively from the launcher-owned trust store's activation
	// record. The payload lock is additionally SCRUBBED on a not-currently-
	// active local-path adoption (a symlinked lock must not redirect the fresh
	// hint write); a URL adoption's lock was just written by
	// clonePack/markPackAdopted — host-authored — so it is kept.
	hint := readPackLock(root)
	hintRemote, hintCommit := strings.TrimSpace(hint.Remote), strings.TrimSpace(hint.Commit)
	if prevRoot != root && remoteURL == "" {
		if serr := scrubUntrustedPackLock(root); serr != nil {
			fmt.Fprintf(out, "pi-stack pack use: %v (refusing to adopt with an untrusted %s in place)\n", serr, packLockName)
			os.Exit(1)
		}
	}

	// Adoption provenance (finding #1, CRITICAL): a pack cloned via a git URL
	// THIS activation, one whose lock carried a Remote marker (fail-safe: a
	// forged marker only RESTRICTS), or one host state / its location under
	// PacksDir proves was cloned (isAdoptedPack), is "adopted" — pack.toml
	// there is attacker-controlled, so shared=false local knowledge refs are
	// never honored (enforced inside resolvePackKnowledgeRef below).
	adopted := remoteURL != "" || hintRemote != "" || isAdoptedPack(root)

	// F5: the Tier-1 trust gate — against TRUSTED HOST STATE (packtruststore.go),
	// never anything the pack ships. Tier-0 (no host-exec facet) adopts
	// silently, exactly as Phase 1 did. Tier-1 halts at the BoM screen unless
	// the trust store holds this pack identity's acceptance of the EXACT
	// current host-exec surface (fingerprint match: MCP argv, host proxy script
	// content, bin pins, egress, creds). Switching between accepted packs never
	// re-prompts; ANY surface change does. Refusal aborts here: nothing
	// registered, installed, or committed.
	//
	// An UNREADABLE trust store is now FATAL (it is the reversibility AND
	// acceptance backbone): proceeding with an empty stand-in would both lose
	// the previous activation's removal set and — at the commit point — clobber
	// the store file with the stand-in. Fail closed with a pointer at the file.
	trustStore, tserr := loadPackTrustStore()
	if tserr != nil {
		fmt.Fprintf(out, "pi-stack pack use: pack trust state unreadable: %v (fix or remove %s and re-run)\n", tserr, packTrustStorePath())
		os.Exit(1)
	}
	// One-time Phase-1 → Phase-2 migration (round-3 #2): a pre-Phase-2 active
	// pack has its activation attribution only in pack.lock — lift it into the
	// (in-memory) store record BEFORE computing the switch below, so its
	// contributions revert correctly. Adopted packs never migrate (their lock
	// is payload); see migratePhase1Activation.
	if prevRoot != "" {
		migratePhase1Activation(trustStore, prevRoot)
	}
	bom := computeHostBoM(p, cfg.GogAccount, localMCPClassifier(env, hostBinaryResolver))
	var bomFingerprint, packKey string
	if bom.tier1() {
		fp, _, ferr := computeHostExecFingerprint(root, bom)
		if ferr != nil {
			fmt.Fprintf(out, "pi-stack pack use: pack %s: %v\n", root, ferr)
			os.Exit(1)
		}
		bomFingerprint = fp
		packKey = trustStore.trustKey(root)
		if got, ok := trustStore.acceptedFingerprint(packKey); !ok || got != fp {
			if gerr := packTrustGate(os.Stdin, out, isTTY(os.Stdin), yes, p.Manifest.Name, bom); gerr != nil {
				fmt.Fprintf(out, "pi-stack pack use: %v\n", gerr)
				os.Exit(1)
			}
		}
	}

	// MCP set (F1 + ADR-1): remove exactly what the PREVIOUS pack's last
	// activation ACTUALLY ADDED (never a user's own manually-added MCP the
	// pack merely re-declares — finding #2), then add what the NEW pack
	// declares. Reversible: pack-use(A) -> pack-use(B) -> pack-use(A) restores
	// cfg.MCP to what it was after the first pack-use(A).
	var removedMCP, removedKnowledge []string
	switch {
	case switching:
		// The previous pack's contribution set: HOST state only (round-2 A).
		removedMCP, removedKnowledge = revertPackPriorContribution(cfg, trustStore.activationFor(prevRoot))
	case prevRoot == root:
		// SAME-pack reactivation (finding D): revert THIS pack's own prior
		// contribution first, then re-apply the manifest fresh below. Without
		// the revert, every Add* returns false (the entries are already live),
		// the new lock overwrites the attribution with EMPTY slices — so a
		// later switch/rm could never remove this pack's contributions — and a
		// field REMOVED from the manifest since the last activation
		// (gog_account, ollama_bridge_model, an mcp, a knowledge ref) would
		// stay live forever. Revert-then-reapply reconciles both: facets still
		// declared re-add just below (regaining attribution), dropped ones
		// stay reverted. The removal set comes from HOST state (round-2 A),
		// never the pack-payload lock — a same-pack `git pull` forgery buys
		// nothing.
		removedMCP, removedKnowledge = revertPackPriorContribution(cfg, trustStore.activationFor(root))
	}
	var addedMCP []string
	for _, m := range packMcpNames(p) {
		if cfg.AddMCP(m) {
			addedMCP = append(addedMCP, m)
		}
	}

	// Knowledge (F4 + F6): add the NEW pack's embedded dir + resolved
	// [[knowledge]] refs (shared travels via resolveBundleRef; private resolves
	// to a local path that never entered the pack's git tree — and is skipped
	// entirely for an adopted pack, finding #1). Only what cfg.AddKnowledgeBundle
	// ACTUALLY ADDED is recorded for the next switch's removal set (finding #2):
	// a bundle already present (added by the user, or by another mechanism) is
	// never claimed as this pack's own contribution.
	var addedKnowledge, newKnowledgeIDs []string
	if p.KnowledgeDir != "" {
		if cfg.AddKnowledgeBundle(p.KnowledgeDir) {
			addedKnowledge = append(addedKnowledge, p.KnowledgeDir)
			newKnowledgeIDs = append(newKnowledgeIDs, canonicalizeKnowledgeBundle(p.KnowledgeDir))
		}
		cfg.AddService("knowledge")
	}
	var skippedPrivate int
	for _, k := range p.Manifest.Knowledge {
		resolved, rerr := resolvePackKnowledgeRef(out, root, adopted, k)
		if rerr != nil {
			if errors.Is(rerr, errPrivateRefSkippedAdopted) {
				skippedPrivate++
				continue
			}
			fmt.Fprintf(out, "note: knowledge %q: %v (skipping)\n", k.Name, rerr)
			continue
		}
		if cfg.AddKnowledgeBundle(resolved) {
			addedKnowledge = append(addedKnowledge, resolved)
			newKnowledgeIDs = append(newKnowledgeIDs, canonicalizeKnowledgeBundle(resolved))
		}
		cfg.AddService("knowledge")
	}

	// Config layering (F4/finding #5): a value the pack declares overwrites, but
	// remembers what cfg held immediately BEFORE the overwrite so switching away
	// restores exactly that (or empty) — never leaking one pack's value into the
	// next. On a SAME-pack reactivation the revert above already restored cfg to
	// the true pre-pack baseline (finding D), so capturing cfg's current value
	// is correct on every path (first use, switch, re-activation) — a chain of
	// re-activations never loses the baseline, and a field the manifest DROPPED
	// stays reverted instead of sticking around.
	var lockGogAccount, lockPriorGogAccount, lockOllamaModel, lockPriorOllamaModel string
	if p.Manifest.GogAccount != "" {
		lockPriorGogAccount = cfg.GogAccount
		lockGogAccount = p.Manifest.GogAccount
		cfg.SetGogAccount(lockGogAccount)
	}
	if m := strings.TrimSpace(p.Manifest.OllamaBridgeModel); m != "" {
		lockPriorOllamaModel = cfg.OllamaBridgeModel
		lockOllamaModel = m
		cfg.OllamaBridgeModel = lockOllamaModel
	}

	cfg.Pack = root

	// COMMIT ORDERING (round-3 R1 + round-4 F1): the lock is written BEFORE
	// cfg.Save, it records the INTENDED contribution set computed above, and a
	// lock-write FAILURE aborts before Save (commitPackActivation) — the config
	// is never committed without its attribution. The two writes can't be one
	// atomic transaction (two files), so pick the safe failure residue: a true
	// crash (SIGKILL/power loss) in the window between lock-write and Save
	// leaves a lock that OVER-claims (it names contributions the config never
	// committed) — harmless, because removal of an absent MCP/bundle is a no-op
	// (config.removeValue tolerates missing entries). The reverse order left
	// the fatal residue: an ACTIVE pack whose config-committed contributions
	// had NO lock attribution, so no later switch/rm could ever remove them.
	//
	// KNOWN RESIDUAL (deliberate, now crash-only): an ORDINARY cfg.Save
	// failure is fully consistent — commitPackActivation snapshots the prior
	// pack.lock and restores it atomically before returning the error, so lock
	// and config never diverge on a plain error (read-only config dir, disk
	// full). The only window left is a TRUE hard kill (SIGKILL/power loss) in
	// the milliseconds between the atomic lock rename and the atomic config
	// rename during a switch/reactivation: the new (narrower) lock lands
	// beside the old config, leaving a dropped MCP/bundle in config with no
	// lock attribution — over-retained until removed by hand (`pi-stack
	// config`), since `pack use`/`pack rm` deliberately remove ONLY what the
	// lock attributes. That scoping is the chosen safe side of the
	// lock-only-removal design: it can never remove a user's manually-added
	// entry (the worse bug fixed in finding #2). Manifest-based reconciliation
	// would reopen that. Over-retention is safe (an extra entry, never a lost
	// one); do NOT "fix" it with manifest-driven removal.
	lock := packLock{
		MCP:                    addedMCP,
		Knowledge:              newKnowledgeIDs,
		Remote:                 remoteURL,
		Commit:                 remoteCommit,
		GogAccount:             lockGogAccount,
		PriorGogAccount:        lockPriorGogAccount,
		OllamaBridgeModel:      lockOllamaModel,
		PriorOllamaBridgeModel: lockPriorOllamaModel,
	}
	if lock.Remote == "" {
		// Not cloned THIS activation: keep whatever adoption marker this pack
		// already carried (a re-activation via local path must not un-adopt it) —
		// the launcher's own host-state provenance first, else the (fail-safe)
		// marker captured off the pack-supplied lock before the scrub. This
		// lands only in the pack.lock HINT (fail-safe marker), never in the
		// host-state activation or acceptance records.
		if prov, ok := trustStore.Adopted[canonicalizePackRoot(root)]; ok {
			lock.Remote, lock.Commit = prov.Remote, prov.Commit
		} else {
			lock.Remote, lock.Commit = hintRemote, hintCommit
		}
	}
	if err := commitPackActivation(cfg, trustStore, root, lock); err != nil {
		// The lock IS part of this switch's committed state (finding #3 +
		// round-4 F1): if it can't be written, NOTHING is committed — the
		// in-memory cfg mutations above are discarded, the on-disk config (and
		// the active pack) stay exactly as they were.
		fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
		os.Exit(1)
	}

	// F5: persist the acceptance in HOST state (the gate above passed, or the
	// stored fingerprint already covered this exact surface — recording is
	// idempotent and re-normalizes provenance). A failed write just means the
	// gate re-prompts next time: fail closed, never open.
	if bom.tier1() {
		// Provenance on the acceptance record is HOST-recorded ONLY (round-2 E):
		// this activation's own clone, or the launcher's adoption record — never
		// the pack-supplied pack.lock, whose forged Remote could alias a legit
		// pack and make recordAcceptance's hygiene sweep DELETE its acceptance.
		// Written via the lock-serialized fresh-load mutation (round-3 #1) so
		// this save can never clobber a concurrent writer's record. The commit
		// stored on the record is provenance METADATA only (round-3 #5): the
		// key is commit-stable, so a new commit with an unchanged host-exec
		// fingerprint never re-prompts.
		rec := packTrustRecord{Path: canonicalizePackRoot(root), Fingerprint: bomFingerprint}
		rec.Remote, rec.Commit = remoteURL, remoteCommit
		if _, werr := mutatePackTrustStore(func(s *packTrustStore) error {
			if rec.Remote == "" {
				if prov, ok := s.Adopted[rec.Path]; ok {
					rec.Remote, rec.Commit = prov.Remote, prov.Commit
				}
			}
			s.recordAcceptance(packKey, rec)
			return nil
		}); werr != nil {
			fmt.Fprintf(out, "note: could not record the accepted host BoM: %v (the Tier-1 gate will re-prompt)\n", werr)
		}
	}

	// --- post-Save: best-effort side effects (each already idempotent). ---

	fmt.Fprintf(out, "active pack -> %s\n", root)
	// On a same-pack reactivation the revert-then-reapply (finding D) removes
	// and immediately re-adds every still-declared entry; report as detached
	// only what actually STAYED out (a facet dropped from the manifest).
	detachedMCP, detachedKnowledge := removedMCP, removedKnowledge
	if !switching {
		detachedMCP, detachedKnowledge = nil, nil
		for _, m := range removedMCP {
			if !containsStr(cfg.MCP, m) {
				detachedMCP = append(detachedMCP, m)
			}
		}
		for _, id := range removedKnowledge {
			if !containsStr(cfg.KnowledgeBundles, id) {
				detachedKnowledge = append(detachedKnowledge, id)
			}
		}
	}
	if len(detachedMCP) > 0 {
		fmt.Fprintf(out, "detached mcp (previous activation): %s\n", strings.Join(detachedMCP, ", "))
	}
	if len(addedMCP) > 0 {
		fmt.Fprintf(out, "attached mcp: %s\n", strings.Join(addedMCP, ", "))
	}
	// finding E: register ALL of this pack's MCPs post-Save (registration is
	// idempotent), never just the newly-added ones — a retry after a failed
	// gateway registration finds the names already in cfg.MCP (AddMCP returned
	// false) and must still re-register, and a pack changing gog_account while
	// redeclaring an existing `gog` server must re-register the new account.
	if all := packMcpNames(p); len(all) > 0 {
		if err := registerServers(cfg, env, out, all, findHostBinary, packContainerMCP(p)); err != nil {
			fmt.Fprintf(out, "note: mcp registration: %v\n", err)
		}
	}
	for _, id := range detachedKnowledge {
		fmt.Fprintf(out, "knowledge bundle detached (previous activation): %s\n", id)
	}
	for _, id := range addedKnowledge {
		fmt.Fprintf(out, "knowledge bundle registered: %s\n", id)
	}
	if skippedPrivate > 0 {
		fmt.Fprintf(out, "skipped %d private knowledge ref(s) from an adopted pack (shared=false local paths are never honored for a pack cloned from a remote)\n", skippedPrivate)
	}

	// F3: swap the host-mode wrappers NOW (live for the next `pi-stack host`):
	// refreshHostPackWrappers clears what host state attributes to the previous
	// activation and stages+verifies+swaps in this pack's ACCEPTED set (the
	// acceptance recorded just above). Best-effort here, like every other
	// post-Save side effect; the strict fingerprint + content re-verification
	// happens again at every host launch.
	if _, werr := refreshHostPackWrappers(out, cfg, false); werr != nil {
		fmt.Fprintf(out, "note: host wrappers not refreshed: %v\n", werr)
	}

	// Solicit any 1Password creds this pack's reference-only integrations need.
	solicitPackCredentials(env, os.Stdin, out, isTTY(os.Stdin), p)

	// (The activation lock — this switch's removal set for the NEXT switch — was
	// already written just BEFORE cfg.Save; see the R1 commit-ordering comment.)

	// A knowledge change is daemon-affecting: restart/advise the running serve so
	// the new bundle is indexed (mirrors `knowledge use`). Best-effort.
	propagateServeConfig(defaultServeReloader(), out)

	// ADR-3: --mcp/--kit are create-only. Print the recreate line UNCONDITIONALLY
	// (this is "the change" for the purposes of packs.md §13's must-fix), so the
	// sandbox-facet-changing case is never silently skipped.
	printPackRecreateLine(out)
}

func runPackRm(out io.Writer, rest []string) {
	if len(rest) > 0 {
		fmt.Fprintf(os.Stderr, "pi-stack pack rm: unexpected argument %q (rm detaches the ACTIVE pack; it takes no name)\n", rest[0])
		os.Exit(2)
	}
	// The ENTIRE detach — re-reading cfg (the active pack) AND the trust
	// store, clearing the wrappers, reverting the contribution set, cfg.Save,
	// and dropping the spent activation record — runs under ONE hold of the
	// cross-process trust lock (concurrency review): `rm` used to decide what
	// to clear from a PRE-lock snapshot, so a concurrent `pi-stack host`
	// wrapper refresh could install + attribute AFTER rm reported "detached",
	// or interleave into a live dir the store attributed to nobody. Under the
	// one lock the refresh and the detach serialize: either rm wins (nothing
	// installed, nothing attributed) or the refresh wins (installed AND
	// attributed) — never installed-but-unattributed. os.Exit stays OUTSIDE
	// the locked fn (withFlock contract); failures return and exit after the
	// lock is released.
	var (
		noActive         bool
		old              string
		removedWrappers  []string
		removedMCP       []string
		removedKnowledge []string
	)
	rmErr := withPackTrustLock(func() error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if cfg.Pack == "" {
			noActive = true
			return nil
		}
		old = cfg.Pack
		// finding #5: `rm` must undo the active pack's contributions (mcp,
		// knowledge, gog/model overrides) too — not just clear cfg.Pack — or
		// "detached" is a lie about what actually happened. The contribution set
		// comes from HOST state (round-2 A), never the pack-payload lock — and an
		// unreadable trust store is FATAL: without it neither the removal set nor
		// the wrapper attribution can be honored.
		store, serr := loadPackTrustStore()
		if serr != nil {
			return fmt.Errorf("pack trust state unreadable: %v (fix or remove %s and re-run)", serr, packTrustStorePath())
		}
		// One-time Phase-1 → Phase-2 migration (round-3 #2), same as `pack use`:
		// a Phase-1 active pack's attribution lives only in its (local) pack.lock.
		migratePhase1Activation(store, old)
		// F3 + round-3 #4: "detached" includes the host wrappers — remove exactly
		// what HOST state attributes to hostPackBinDir() (reliable even when the
		// pack directory itself is gone; attribution never lived in the pack), and
		// do it FIRST: a failed clear aborts with a non-zero exit BEFORE anything
		// detaches, so `pack rm` never claims success while host executables
		// remain, and a plain re-run retries the whole detach cleanly. The
		// attribution is discarded only on CONFIRMED removal. Acceptance is kept:
		// trust was granted at adoption and re-attaching must not re-prompt.
		if store.Installed != nil && len(store.Installed.Wrappers) > 0 {
			removedWrappers = append([]string(nil), store.Installed.Wrappers...)
			if cerr := clearInstalledHostPackWrappersLocked(out, store); cerr != nil {
				removedWrappers = nil
				return fmt.Errorf("host wrappers could not be removed: %v — nothing detached; fix that and re-run (a `pi-stack host` launch refuses until they are cleared)", cerr)
			}
		}
		removedMCP, removedKnowledge = revertPackPriorContribution(cfg, store.activationFor(old))
		cfg.Pack = ""
		if err := cfg.Save(); err != nil {
			return err
		}
		// The activation record is spent (its contributions were just reverted).
		// Dropped only when it was attributed to THIS pack, via the fresh-load
		// already-locked mutation (round-3 #1; the lock is held — never nest
		// withPackTrustLock); a failed store write merely over-claims (removals
		// of absent entries are no-ops).
		if store.hasActivationFor(old) {
			if _, werr := mutatePackTrustStoreLocked(func(s *packTrustStore) error {
				if s.hasActivationFor(old) {
					s.Activation = nil
				}
				return nil
			}); werr != nil {
				fmt.Fprintf(out, "note: could not clear the activation record: %v (harmless over-claim; re-run `pack rm` once %s is writable)\n", werr, packTrustStorePath())
			}
		}
		return nil
	})
	if rmErr != nil {
		fmt.Fprintf(out, "pi-stack pack rm: %v\n", rmErr)
		os.Exit(1)
	}
	if noActive {
		fmt.Fprintln(out, "no active pack to detach")
		return
	}
	fmt.Fprintf(out, "detached active pack (%s). The files are untouched; re-attach with `pi-stack pack use`.\n", old)
	if len(removedWrappers) > 0 {
		fmt.Fprintf(out, "removed host wrappers: %s\n", strings.Join(removedWrappers, ", "))
	}
	if len(removedMCP) > 0 {
		fmt.Fprintf(out, "detached mcp: %s\n", strings.Join(removedMCP, ", "))
	}
	for _, id := range removedKnowledge {
		fmt.Fprintf(out, "knowledge bundle detached: %s\n", id)
	}
	if len(removedKnowledge) > 0 {
		propagateServeConfig(defaultServeReloader(), out)
	}
	if len(removedMCP) > 0 {
		printPackRecreateLine(out)
	}
}

// --- git-URL adoption -------------------------------------------------------

// isPackGitURL reuses knowledge.go's isGitURL and additionally accepts the
// "git+" scheme prefix used by kit URLs.
func isPackGitURL(s string) bool {
	s = strings.TrimSpace(s)
	// A git transport-helper string (ext::, fd::, ...) is URL-SHAPED, not a local
	// path: route it here so clonePack's safeGitURL rejects it with a clear
	// "unsafe transport" message instead of a confusing "not a pack" path error.
	if strings.Contains(s, "::") {
		return true
	}
	return strings.HasPrefix(s, "git+") || isGitURL(s)
}

// parsePackURL splits an optional "#ref=<ref>" (or bare "#<ref>") pin off a git
// URL and strips a leading "git+" scheme prefix. Returns (url, ref).
func parsePackURL(raw string) (url, ref string) {
	url = strings.TrimPrefix(raw, "git+")
	if i := strings.IndexByte(url, '#'); i >= 0 {
		frag := url[i+1:]
		url = url[:i]
		ref = strings.TrimPrefix(frag, "ref=")
	}
	return url, ref
}

// packNameFromURL derives a SAFE, stable local dir name from a git URL: the
// basename (minus .git), sanitized to [A-Za-z0-9._-] with any path-traversal
// (`.`/`..`/empty) neutralized, plus a short hash of the FULL url so two remotes
// with the same basename (org-a/tools vs org-b/tools) never collide on one dest.
func packNameFromURL(url string) string {
	u := strings.TrimSuffix(url, ".git")
	u = strings.TrimRight(u, "/")
	base := u
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		base = u[i+1:]
	}
	safe := make([]rune, 0, len(base))
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			safe = append(safe, r)
		default:
			safe = append(safe, '-')
		}
	}
	name := strings.Trim(string(safe), ".-")
	if name == "" || name == ".." {
		name = "pack"
	}
	sum := sha256.Sum256([]byte(url))
	return name + "-" + hex.EncodeToString(sum[:])[:16]
}

// safeGitURL rejects git URLs whose transport can execute arbitrary commands or
// read arbitrary host files: only https/http/ssh/git protocols and scp-style
// `user@host:path` are allowed. `ext::`, `file://`, a leading `-` (arg
// injection), and anything else are refused. v1 packs are Tier-0 (no shipped
// executables), so a clone must not become a code-execution vector.
func safeGitURL(url string) bool {
	if url == "" || strings.HasPrefix(url, "-") {
		return false
	}
	if strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://") ||
		strings.HasPrefix(url, "ssh://") || strings.HasPrefix(url, "git://") {
		return true
	}
	// scp-style user@host:path (no scheme). Must contain ':' and not be a
	// transport helper like ext::/fd:: (those contain '::').
	if strings.Contains(url, "::") {
		return false
	}
	if at := strings.IndexByte(url, '@'); at > 0 && strings.Contains(url[at:], ":") {
		return true
	}
	return false
}

// clonePack clones (or updates) a remote pack into PacksDir/<name>, pinned to the
// optional ref, and returns the local path. SHA-pin/provenance is a v2 concern;
// v1 trusts the git remote (Tier 0: skills/knowledge/config, no host execution).
func clonePack(env shellEnv, out io.Writer, raw string) (string, error) {
	if env.run == nil {
		return "", fmt.Errorf("git not available")
	}
	url, ref := parsePackURL(raw)
	if !safeGitURL(url) {
		return "", fmt.Errorf("refusing unsafe git URL %q (only https/ssh/git remotes; no ext::/file:: transports)", url)
	}
	if ref != "" && strings.HasPrefix(ref, "-") {
		return "", fmt.Errorf("refusing ref %q (leading dash)", ref)
	}
	name := packNameFromURL(url)
	dest := filepath.Join(config.PacksDir(), name)
	if err := os.MkdirAll(config.PacksDir(), 0o755); err != nil {
		return "", err
	}
	freshClone := false
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		// A dir already exists at this URL-hash dest. Verify its origin actually
		// matches the requested URL before trusting it — a 64-bit hash collision (or
		// a pre-planted dir) must NOT let us fetch/activate the wrong repo. On
		// mismatch, wipe and re-clone.
		if got, _ := env.run("git", "-C", dest, "remote", "get-url", "origin"); strings.TrimSpace(got) != url {
			_ = os.RemoveAll(dest)
		} else {
			fmt.Fprintf(out, "updating pack %q...\n", name)
			if _, err := env.run("git", "-C", dest, "fetch", "--tags", "--", "origin"); err != nil {
				return "", fmt.Errorf("git fetch %s: %w", url, err)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); err != nil {
		fmt.Fprintf(out, "cloning pack %q from %s...\n", name, url)
		if _, err := env.run("git", "clone", "--", url, dest); err != nil {
			return "", fmt.Errorf("git clone %s: %w", url, err)
		}
		freshClone = true
	}
	if ref != "" {
		// No `--` before a ref: `git checkout -- <ref>` means path-checkout, not a
		// ref switch. ref is already validated (no leading dash), so this is safe.
		if _, err := env.run("git", "-C", dest, "checkout", ref); err != nil {
			if freshClone {
				_ = os.RemoveAll(dest)
			}
			return "", fmt.Errorf("git checkout %s: %w", ref, err)
		}
		// Advance to the fetched tip when ref is a branch (no-op for a tag/sha).
		_, _ = env.run("git", "-C", dest, "reset", "--hard", "origin/"+ref)
	} else if !freshClone {
		// Unpinned existing clone: advance to the remote default branch's tip.
		_, _ = env.run("git", "-C", dest, "reset", "--hard", "@{upstream}")
	}
	// A clone that has no pack.toml is not a pack: clean up the fresh clone so a
	// retry starts clean, and fail with a clear message.
	if _, err := os.Stat(filepath.Join(dest, packManifestName)); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", fmt.Errorf("cloned %s but it has no %s — not a pack", url, packManifestName)
	}
	// pack.lock is LOCAL GENERATED activation state (ADR-1) and must NEVER come
	// from the remote (round-3 S1, CRITICAL): a malicious pack could commit it
	// as a SYMLINK (-> /dev/null, or a host file) so the adoption marker written
	// below would land on the symlink's TARGET — un-adopting the pack (bypassing
	// the local-path knowledge guard) and/or overwriting an arbitrary host file.
	// A checked-in REGULAR pack.lock is just as hostile (its attribution fields
	// would be merged by markPackAdopted and could claim the user's own MCP
	// entries for removal on switch-away). Scrub it AFTER every git operation
	// above (checkout/reset --hard restore tracked files) and BEFORE
	// markPackAdopted writes the real one. Failing the scrub fails the adoption.
	if err := scrubRemotePackLock(env, dest, freshClone); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", err
	}
	// Provenance durability (finding B): mark the clone ADOPTED here — durably,
	// before returning — never leaving it to the caller's post-Save lock
	// rewrite. A cfg.Save()/lock-write failure after this return must not leave
	// an UNMARKED adopted clone on disk that a retry would treat as user-
	// authored (and so honor its private/local knowledge refs — the finding-A
	// guard keys on this marker). If the marker itself cannot be written, fail
	// the whole adoption: an unmarked adopted clone is exactly the state this
	// guard exists to prevent.
	if err := markPackAdopted(env, dest, url); err != nil {
		if freshClone {
			_ = os.RemoveAll(dest)
		}
		return "", fmt.Errorf("recording adoption provenance for %s: %w", url, err)
	}
	return dest, nil
}

// scrubRemotePackLock deletes a pack.lock that came from the REMOTE in a
// cloned pack tree (round-3 S1): on a fresh clone ANY pack.lock came from the
// remote; on an update, one that is a symlink (never legitimate — writePackLock
// only ever creates regular files) or that git tracks (checkout/reset restore
// it from the remote) is remote-authored. A legit LOCAL lock (untracked regular
// file carrying prior activation attribution) is preserved. os.Remove removes a
// symlink itself, never its target.
func scrubRemotePackLock(env shellEnv, dest string, freshClone bool) error {
	path := packLockPath(dest)
	fi, err := os.Lstat(path)
	if err != nil {
		return nil // no pack.lock at all — nothing to scrub
	}
	fromRemote := freshClone || fi.Mode()&os.ModeSymlink != 0
	if !fromRemote && env.run != nil {
		// Tracked by git => restored from the remote by checkout/reset above.
		if _, lerr := env.run("git", "-C", dest, "ls-files", "--error-unmatch", "--", packLockName); lerr == nil {
			fromRemote = true
		}
	}
	if !fromRemote {
		return nil
	}
	if rerr := os.Remove(path); rerr != nil && !os.IsNotExist(rerr) {
		return fmt.Errorf("removing checked-in %s: %w", packLockName, rerr)
	}
	return nil
}

// markPackAdopted durably records adoption provenance (pack.lock Remote +
// Commit) on a cloned/updated pack (finding B). It MERGES into any existing
// lock so a re-clone or update never sheds earlier activation attribution.
// The same provenance is mirrored into the launcher-owned trust store (host
// state — the trusted source for pack identity and the adopted-pack guard);
// that mirror is best-effort because the lock marker plus the under-PacksDir
// location check already keep the guard fail-safe without it.
func markPackAdopted(env shellEnv, root, remote string) error {
	lock := readPackLock(root)
	lock.Remote = remote
	lock.Commit = ""
	if env.run != nil {
		if sha, err := env.run("git", "-C", root, "rev-parse", "HEAD"); err == nil {
			lock.Commit = strings.TrimSpace(sha)
		}
	}
	_ = recordPackAdoptionInTrustStore(root, remote, lock.Commit)
	return writePackLock(root, lock)
}

// --- helpers ----------------------------------------------------------------

// writePackManifest writes root's pack.toml. LEAF-symlink-safe (mirrors
// writePackLock/writePackLockBytes): the pack root is untrusted input — an
// adopted or migrated pack could have pack.toml replaced with a symlink (e.g.
// pointing outside the pack root) — so this Lstat-REFUSES a symlinked
// destination outright, then writes ATOMICALLY via a same-dir temp file +
// rename (an interrupted write can never truncate/corrupt an existing
// manifest).
func writePackManifest(root string, m packManifest) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	dest := filepath.Join(root, packManifestName)
	if fi, err := os.Lstat(dest); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to write through it", dest)
	}
	return atomicWriteInDir(root, packManifestName, buf.Bytes(), 0o644)
}

// seedPackGitignore appends a `pack.lock` line to <root>/.gitignore, creating
// the file if absent, so a fresh pack never accidentally commits its generated
// activation-provenance lockfile (ADR-1). Idempotent (checked by substring) and
// best-effort: a write failure is silent — it must never block `pack new`.
//
// Symlink-safe (mirrors writePackLockBytes): `pack new .` can run in an
// UNTRUSTED directory, and os.ReadFile/os.WriteFile FOLLOW symlinks — a
// .gitignore symlinked at e.g. ~/.bashrc would have pack.lock appended to the
// TARGET. Lstat-REFUSE a symlinked .gitignore outright, and write via the
// same-dir atomic temp+rename (rename replaces a symlink, never follows one)
// so there is no check-then-write window either.
func seedPackGitignore(root string) {
	path := filepath.Join(root, ".gitignore")
	const line = packLockName
	if isSymlinkPath(path) {
		return // never read or write through a symlinked .gitignore
	}
	b, err := os.ReadFile(path)
	if err == nil && strings.Contains(string(b), line) {
		return // already present
	}
	content := string(b)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line + "\n"
	_ = atomicWriteInDir(root, ".gitignore", []byte(content), 0o644)
}

func present(p string) string {
	if p == "" {
		return "(none)"
	}
	return p
}

func skillTemplate(name string) string {
	return "---\nname: " + name + "\ndescription: \"TODO: when should this skill fire? One sentence.\"\n---\n# " + name + "\n\nTODO: tight, opinionated steps.\n"
}

func knowledgeTemplate(name string) string {
	return "# " + name + "\n\nTODO: durable, shared domain knowledge (what is X and why), not a personal preference.\n"
}

const packUsage = `usage: pi-stack pack <new|add|ls|show|use|rm>

A pack is a git-backed bundle of skills + knowledge + mcp integrations + proxy
wrappers + config that defines your context. See docs/design/packs.md.

  new [PATH]              adopt an existing repo (or one with pack.toml) as a
                          pack, else git-init a fresh one. Default PATH is the
                          default pack root (~/.local/share/pi-stack/default).
  add skill <name> [P]              add a skill doc
  add knowledge <name> [P]          add an embedded knowledge doc
  add knowledge <name> [P] --ref <git-url|path> [--private]
                                     add a knowledge REFERENCE instead of
                                     embedding: shared (default) travels with
                                     the pack; --private does not
  add proxy <name> [P] [--host]     scaffold bin/<name> (an in-sandbox CLI
                                     wrapper on PATH); --host makes it a
                                     HOST-mode wrapper instead: Tier-1, gated
                                     by the "pack use" BoM review, on PATH for
                                     "pi-stack host" only
  add mcp <name> [P] [--env VAR]    declare an MCP server this pack needs +
                                     the op-refs.env credential var name
                                     (attaching to the ACTIVE pack is Tier-1:
                                     the host BoM gate fires; --yes accepts)
                          (all "add" forms implicit-create pack P; default P
                          is the default pack)
  ls                      show the active pack
  show [PATH]             inspect a pack (default: the active pack)
  use [--yes] <path|git-url|default>
                          set the active pack: swaps mcp/knowledge/config in
                          ONE transaction (pack.lock tracks what to remove on
                          the next switch); a git URL is cloned to
                          ~/.local/share/pi-stack/packs/<name> (optional #ref pin).
                          "default" is a built-in alias for the default pack
                          root (not $PWD/default); "personal" also works as a
                          deprecated alias (prints a deprecation warning).
                          A pack with HOST-exec facets (mcp, host wrappers,
                          [[bin]]) is Tier-1: adoption halts at a host
                          bill-of-materials review ([y/N], default No);
                          non-TTY fails closed unless --yes. MCP attach +
                          sandbox bin/ wrappers need a recreate
                          (pi-stack run --replace) to take effect.
  rm                      detach the active pack (files untouched)
`
