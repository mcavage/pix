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
}

// packInfo is a resolved pack on disk.
type packInfo struct {
	Root         string
	Manifest     packManifest
	SkillsDir    string // <root>/skills if it exists, else ""
	KnowledgeDir string // <root>/knowledge if it exists, else ""
	BinDir       string // <root>/bin if it exists, else "" (F2/F3 proxy wrapper scripts)
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

// personalPackRoot is the default personal-pack location.
func personalPackRoot() string { return config.PackDir() }

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
// as launch-aborting). The CONFIGURED active pack (cfg.Pack, or the personal
// pack fallback) fails CLOSED too when it exists but won't load — a symlink
// rejection, facet-validation failure, or parse error means a broken or
// TAMPERED pack, and launching without its declared wrappers/skills would be
// a silent downgrade. The ONLY degradable case is errNotAPack ("genuinely
// absent": the dir or its pack.toml is gone), which warns once and proceeds
// as if no pack were active. A declared-but-unbuildable sandbox proxy is ALSO
// fatal (round-4 F2): the launch fails CLOSED rather than creating a sandbox
// missing a declared wrapper.
func applyPackToLaunch(cfg *config.Config, o *runOpts, env shellEnv) error {
	packRoot := activePackRoot(cfg.Pack, o.Pack)
	if packRoot == "" {
		return nil // no active pack; nothing to mount (detached or never created)
	}
	p, err := loadPack(packRoot)
	if err != nil {
		if strings.TrimSpace(o.Pack) != "" {
			return fmt.Errorf("--pack %s: %v", o.Pack, err)
		}
		if errors.Is(err, errNotAPack) {
			// Genuinely absent (deleted dir / no pack.toml): warn and launch
			// without it, as if no pack were active. Not fatal — a stale
			// cfg.Pack must not brick every launch.
			fmt.Fprintf(os.Stderr, "pi-stack: active pack unavailable (%v); launching without it — `pi-stack pack use <path>` to re-point it or `pi-stack pack rm` to detach\n", err)
			return nil
		}
		// The pack EXISTS but won't load (symlink injected, validation/parse
		// failure): fail the launch closed. Creating a sandbox from a broken or
		// tampered active pack would silently drop its declared context.
		return fmt.Errorf("active pack %s: %v (refusing to launch without the pack's declared context; fix the pack or `pi-stack pack rm` to detach it)", packRoot, err)
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
		return fmt.Errorf("pack %s: %v (refusing to launch a sandbox missing a declared wrapper; fix the pack's bin/ or drop the [[proxy]] entry)", p.Manifest.Name, kerr)
	}
	if kit != "" && !containsStr(o.PackKits, kit) {
		o.PackKits = append(o.PackKits, kit)
	}
	for _, ig := range p.Manifest.Integrations {
		if ig.Env != "" && !opRefFilled(env, ig.Env) {
			fmt.Fprintf(os.Stderr, "pi-stack: pack integration %q needs a credential — set it: pi-stack secret set %s op://vault/item/field\n", ig.Name, ig.Env)
		}
	}
	return nil
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
// spec.yaml, plus files/usr/local/bin/<name> (0755) for each [[proxy]] with
// Host unset/false — /usr/local/bin is already on the DHI image's PATH, so the
// wrapper needs no in-VM shim. Returns (dir, nil) on success, ("", nil) when
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
	if len(sandboxProxies) == 0 {
		// No sandbox proxies: nothing to mount. A previous launch's kit dir is
		// inert (nothing references it) and the sweep above cleans it up.
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
	binOut := filepath.Join(dir, "files", "usr", "local", "bin")
	if err := os.MkdirAll(binOut, 0o755); err != nil {
		return fail("pack kit for %s: %v", p.Manifest.Name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte("kind: mixin\n"), 0o644); err != nil {
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
	tmp, err := os.CreateTemp(root, packLockName+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func(werr error) error {
		tmp.Close()
		_ = os.Remove(tmpName)
		return werr
	}
	if _, err := tmp.Write(data); err != nil {
		return cleanup(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil { // CreateTemp makes 0600
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// commitPackActivation is the two-file commit point shared by `pack use` and
// the active-pack `pack add mcp` path: write the attribution lock FIRST, then
// cfg.Save. A lock-write failure ABORTS before Save (round-4 F1): committing
// the config without its attribution would strand MCP/knowledge/config
// entries that no later `pack use`/`pack rm` could ever remove (removal is
// deliberately scoped to lock attribution ONLY — see the commit-ordering
// comment in runPackUse). On abort nothing is committed: the mutated cfg was
// in-memory only, so the on-disk config is unchanged.
//
// A cfg.Save FAILURE (an ordinary error — read-only config dir, disk full —
// not a crash) ROLLS the lock BACK: the prior pack.lock bytes are snapshotted
// before the new lock is written and restored atomically before returning the
// error, so the on-disk lock and config stay mutually consistent. Without the
// rollback, a same-pack reactivation that DROPPED a contribution would leave
// the new (narrower) lock beside the old config — stranding the dropped entry
// with no attribution, unremovable by `pack rm`. See the KNOWN RESIDUAL
// comment in runPackUse for the only inconsistency window left (a hard kill
// between the two renames).
func commitPackActivation(cfg *config.Config, root string, lock packLock) error {
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
	if err := cfg.Save(); err != nil {
		// Roll the lock back so it matches the (unchanged) on-disk config.
		var rerr error
		if priorExists {
			rerr = writePackLockBytes(root, priorLock)
		} else {
			rerr = os.Remove(packLockPath(root))
		}
		if rerr != nil {
			return fmt.Errorf("saving config: %v (and restoring the prior pack.lock failed: %v — the lock may over-claim this activation's contributions; harmless, but re-run `pack use` once the config is writable)", err, rerr)
		}
		return fmt.Errorf("saving config: %v (pack.lock rolled back; nothing was committed)", err)
	}
	return nil
}

// isAdoptedPack reports whether root's pack.lock carries adoption provenance
// (a non-empty Remote), i.e. this pack was cloned from a remote via `pack use
// <git-url>` at some point (clonePack/markPackAdopted set it, and it survives
// every later re-activation). Used by the finding-#1 CRITICAL guard: a
// shared=false local-path [[knowledge]] reference is NEVER honored for an
// adopted pack, because pack.toml there is attacker-controlled input from the
// remote — honoring it would let a malicious pack.toml point AddKnowledgeBundle
// at an arbitrary host directory (e.g. ~/.ssh) that the sandbox can then read
// via the knowledge service.
func isAdoptedPack(root string) bool {
	return strings.TrimSpace(readPackLock(root).Remote) != ""
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
// old unconditional profile-delete in run.go.
func writeMemoryScope(workspace string, p *packInfo) {
	dir := filepath.Join(workspace, ".pi-stack")
	if p == nil {
		_ = os.Remove(filepath.Join(dir, "profile"))
		return
	}
	scope := strings.TrimSpace(p.Manifest.MemoryScope)
	if scope == "" {
		scope = strings.TrimSpace(p.Manifest.Name)
	}
	if scope == "" || scope == "default" {
		_ = os.Remove(filepath.Join(dir, "profile"))
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, "profile"), []byte(scope+"\n"), 0o644)
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
func writePackContextFiles(cfg *config.Config, o runOpts) {
	writeOllamaBridgeFile(o.Workspace, cfg.OllamaBridgeModel)
	var activePack *packInfo
	if root := activePackRoot(cfg.Pack, o.Pack); root != "" {
		if lp, lerr := loadPack(root); lerr == nil {
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
// the personal pack root.
func packTarget(rest []string) string {
	if len(rest) > 0 && strings.TrimSpace(rest[0]) != "" {
		return expandUser(rest[0])
	}
	return personalPackRoot()
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

// activatePersonalPack sets the personal pack as active (config `pack`) if no
// pack is active yet, so implicit-create makes it immediately usable (no manual
// `pack use`). Best-effort. Only for the personal pack root.
func activatePersonalPack(root string) {
	if root != personalPackRoot() {
		return
	}
	cfg, err := config.Load()
	if err != nil || cfg.Pack != "" {
		return
	}
	cfg.Pack = root
	_ = cfg.Save()
}

// runPackNew adopts a pre-existing repo (or one already carrying pack.toml) in
// place, else creates + git-inits a fresh pack. Never re-inits or clobbers.
func runPackNew(env shellEnv, out io.Writer, rest []string) {
	root := packTarget(rest)
	// Already a pack? Nothing to do (but ensure the personal one is active).
	if _, err := os.Stat(filepath.Join(root, packManifestName)); err == nil {
		fmt.Fprintf(out, "already a pack: %s\n", root)
		activatePersonalPack(root)
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
	// Auto-activate the personal pack so it is immediately usable (no manual
	// `pack use` for the common case). A named/other pack still needs `pack use`.
	if root == personalPackRoot() {
		activatePersonalPack(root)
		fmt.Fprintln(out, "active pack -> this (personal) pack")
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
	var host, private bool
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
	root := personalPackRoot()
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
			// F3 (host-mode wrappers) is P2: the struct field is carried, but
			// installation into the host agent dir's PATH is not wired in this build.
			fmt.Fprintln(out, "note: host=true wrappers are a Phase-2 facet (host-mode PATH install is not yet wired).")
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
			added := cfg.AddMCP(name)
			if added {
				// Attribution stays gated on the AddMCP result (finding #2): a
				// pre-existing, user-added name is never claimed as this pack's.
				// Lock BEFORE Save, ABORT on lock failure (round-3 R1 + round-4 F1,
				// same commit point as runPackUse): the config is never committed
				// without its attribution, so a later `pack use`/`pack rm` can
				// always clean up what this command added.
				lock := readPackLock(root)
				if !containsStr(lock.MCP, name) {
					lock.MCP = append(lock.MCP, name)
				}
				if err := commitPackActivation(cfg, root, lock); err != nil {
					fmt.Fprintf(out, "pi-stack pack add: %v\n", err)
					os.Exit(1)
				}
			}
			// finding E: registration runs even when the name was ALREADY in
			// cfg.MCP (added == false) — it is idempotent, and a retry after a
			// failed gateway registration must actually re-register instead of
			// silently doing nothing.
			if err := registerServers(cfg, env, out, []string{name}, findHostBinary); err != nil {
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
			cred := ""
			if ig.Env != "" {
				if opRefFilled(env, ig.Env) {
					cred = ig.Env + " ✓"
				} else {
					cred = ig.Env + " ✗ (run: pi-stack secret set " + ig.Env + " op://vault/item/field)"
				}
			}
			fmt.Fprintf(out, "  - %s", ig.Name)
			if ig.MCP != "" {
				fmt.Fprintf(out, " (mcp: %s)", ig.MCP)
			}
			if cred != "" {
				fmt.Fprintf(out, " — %s", cred)
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
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: pi-stack pack use <path|git-url>")
		os.Exit(2)
	}
	arg := strings.TrimSpace(rest[0])
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
	// selfLock is THIS pack's own lock as it existed before this activation
	// overwrites it — read up front so the adoption marker (Remote/Commit) is
	// preserved across a re-activation of the SAME pack (see below).
	selfLock := readPackLock(root)

	// Adoption provenance (finding #1, CRITICAL): a pack cloned via a git URL
	// THIS activation, or one whose lock already carries a Remote from a PRIOR
	// clone (re-activating the same clone by local path), is "adopted" —
	// pack.toml there is attacker-controlled, so shared=false local knowledge
	// refs are never honored (enforced inside resolvePackKnowledgeRef below).
	adopted := remoteURL != "" || isAdoptedPack(root)

	// MCP set (F1 + ADR-1): remove exactly what the PREVIOUS pack's last
	// activation ACTUALLY ADDED (never a user's own manually-added MCP the
	// pack merely re-declares — finding #2), then add what the NEW pack
	// declares. Reversible: pack-use(A) -> pack-use(B) -> pack-use(A) restores
	// cfg.MCP to what it was after the first pack-use(A).
	var removedMCP, removedKnowledge []string
	switch {
	case switching:
		removedMCP, removedKnowledge = revertPackPriorContribution(cfg, readPackLock(prevRoot))
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
		// stay reverted.
		removedMCP, removedKnowledge = revertPackPriorContribution(cfg, selfLock)
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
		// already carried (a re-activation via local path must not un-adopt it).
		lock.Remote = selfLock.Remote
		lock.Commit = selfLock.Commit
	}
	if err := commitPackActivation(cfg, root, lock); err != nil {
		// The lock IS part of this switch's committed state (finding #3 +
		// round-4 F1): if it can't be written, NOTHING is committed — the
		// in-memory cfg mutations above are discarded, the on-disk config (and
		// the active pack) stay exactly as they were.
		fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
		os.Exit(1)
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
		if err := registerServers(cfg, env, out, all, findHostBinary); err != nil {
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
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pi-stack pack rm: %v\n", err)
		os.Exit(1)
	}
	if cfg.Pack == "" {
		fmt.Fprintln(out, "no active pack to detach")
		return
	}
	old := cfg.Pack
	// finding #5: `rm` must undo the active pack's contributions (mcp,
	// knowledge, gog/model overrides) too — not just clear cfg.Pack — or
	// "detached" is a lie about what actually happened.
	oldLock := readPackLock(old)
	removedMCP, removedKnowledge := revertPackPriorContribution(cfg, oldLock)
	cfg.Pack = ""
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "pi-stack pack rm: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "detached active pack (%s). The files are untouched; re-attach with `pi-stack pack use`.\n", old)
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
func markPackAdopted(env shellEnv, root, remote string) error {
	lock := readPackLock(root)
	lock.Remote = remote
	lock.Commit = ""
	if env.run != nil {
		if sha, err := env.run("git", "-C", root, "rev-parse", "HEAD"); err == nil {
			lock.Commit = strings.TrimSpace(sha)
		}
	}
	return writePackLock(root, lock)
}

// --- helpers ----------------------------------------------------------------

func writePackManifest(root string, m packManifest) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(m); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(root, packManifestName), buf.Bytes(), 0o644)
}

// seedPackGitignore appends a `pack.lock` line to <root>/.gitignore, creating
// the file if absent, so a fresh pack never accidentally commits its generated
// activation-provenance lockfile (ADR-1). Idempotent (checked by substring) and
// best-effort: a write failure is silent — it must never block `pack new`.
func seedPackGitignore(root string) {
	path := filepath.Join(root, ".gitignore")
	const line = packLockName
	b, err := os.ReadFile(path)
	if err == nil && strings.Contains(string(b), line) {
		return // already present
	}
	content := string(b)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += line + "\n"
	_ = os.WriteFile(path, []byte(content), 0o644)
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
                          personal pack root (~/.local/share/pi-stack/skills).
  add skill <name> [P]              add a skill doc
  add knowledge <name> [P]          add an embedded knowledge doc
  add knowledge <name> [P] --ref <git-url|path> [--private]
                                     add a knowledge REFERENCE instead of
                                     embedding: shared (default) travels with
                                     the pack; --private does not
  add proxy <name> [P] [--host]     scaffold bin/<name> (an in-sandbox CLI
                                     wrapper on PATH); --host marks it a
                                     Phase-2 host-mode wrapper (not yet wired)
  add mcp <name> [P] [--env VAR]    declare an MCP server this pack needs +
                                     the op-refs.env credential var name
                          (all "add" forms implicit-create pack P; default P
                          is the personal pack)
  ls                      show the active pack
  show [PATH]             inspect a pack (default: the active pack)
  use <path|git-url>      set the active pack: swaps mcp/knowledge/config in
                          ONE transaction (pack.lock tracks what to remove on
                          the next switch); a git URL is cloned to
                          ~/.local/share/pi-stack/packs/<name> (optional #ref pin).
                          MCP attach + sandbox bin/ wrappers need a recreate
                          (pi-stack run --replace) to take effect.
  rm                      detach the active pack (files untouched)
`
