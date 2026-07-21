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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// loadPack reads a pack from a directory. A missing pack.toml is an error (the
// presence of pack.toml is the entire "is this a pack" test).
func loadPack(root string) (*packInfo, error) {
	root = filepath.Clean(root)
	mf := filepath.Join(root, packManifestName)
	b, err := os.ReadFile(mf)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%s is not a pack (no %s)", root, packManifestName)
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
	}
	return nil
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
// --pack that fails to load is fatal; the personal/active fallback failing is a
// silent skip.
func applyPackToLaunch(cfg *config.Config, o *runOpts, env shellEnv) {
	packRoot := activePackRoot(cfg.Pack, o.Pack)
	if packRoot == "" {
		return // no active pack; nothing to mount (detached or never created)
	}
	p, err := loadPack(packRoot)
	if err != nil {
		if strings.TrimSpace(o.Pack) != "" {
			fmt.Fprintf(os.Stderr, "pi-stack: --pack %s: %v\n", o.Pack, err)
			os.Exit(1)
		}
		return
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
	// escape hatch would silently drop the base image kit).
	if kit := synthesizePackKit(p, os.Stderr); kit != "" && !containsStr(o.PackKits, kit) {
		o.PackKits = append(o.PackKits, kit)
	}
	for _, ig := range p.Manifest.Integrations {
		if ig.Env != "" && !opRefFilled(env, ig.Env) {
			fmt.Fprintf(os.Stderr, "pi-stack: pack integration %q needs a credential — set it: pi-stack secret set %s op://vault/item/field\n", ig.Name, ig.Env)
		}
	}
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

// packKitDir resolves the ephemeral mixin-kit dir a pack's sandbox bin/
// wrappers are synthesized into: <StateDir>/pi-stack/pack-kits/<hash>/, keyed by
// a hash of the pack root so re-launching the same pack overwrites in place
// (bounded disk use — see packs-v2-impl.md §8's "ephemeral pack-kit dir
// lifecycle" risk note; a `pi-stack state reset` sweep is the cleanup path).
func packKitDir(root string) string {
	sum := sha256.Sum256([]byte(root))
	dir, err := config.StateDir()
	if err != nil {
		dir = "pi-stack-state"
	}
	return filepath.Join(dir, "pi-stack", "pack-kits", hex.EncodeToString(sum[:])[:16])
}

// synthesizePackKit builds (or refreshes, overwrite-in-place) the ephemeral
// mixin kit that puts a pack's non-host bin/ wrappers on the sandbox PATH
// (F2/ADR-2): a minimal `kind: mixin` spec.yaml, plus
// files/usr/local/bin/<name> (0755) for each [[proxy]] with Host unset/false
// — /usr/local/bin is already on the DHI image's PATH, so the wrapper needs no
// in-VM shim. Returns the kit dir, or "" when the pack has no sandbox proxies
// (nothing to mount — the caller must not stack an empty kit). Copies (never
// symlinks): loadPack already refuses a symlinked bin/, and sbx mounts a real
// tree. Best-effort: an I/O failure warns to out and skips that one wrapper (or
// returns "" on a directory-level failure) rather than failing the launch.
func synthesizePackKit(p *packInfo, out io.Writer) string {
	var sandboxProxies []packProxy
	for _, pr := range p.Manifest.Proxies {
		if !pr.Host {
			sandboxProxies = append(sandboxProxies, pr)
		}
	}
	if len(sandboxProxies) == 0 {
		return ""
	}
	dir := packKitDir(p.Root)
	binOut := filepath.Join(dir, "files", "usr", "local", "bin")
	if err := os.MkdirAll(binOut, 0o755); err != nil {
		fmt.Fprintf(out, "pi-stack: pack kit for %s: %v\n", p.Manifest.Name, err)
		return ""
	}
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"), []byte("kind: mixin\n"), 0o644); err != nil {
		fmt.Fprintf(out, "pi-stack: pack kit for %s: %v\n", p.Manifest.Name, err)
		return ""
	}
	for _, pr := range sandboxProxies {
		src := filepath.Join(p.Root, "bin", pr.Name)
		b, err := os.ReadFile(src)
		if err != nil {
			fmt.Fprintf(out, "pi-stack: pack proxy %q: %v (skipping)\n", pr.Name, err)
			continue
		}
		if err := os.WriteFile(filepath.Join(binOut, pr.Name), b, 0o755); err != nil {
			fmt.Fprintf(out, "pi-stack: pack proxy %q: %v (skipping)\n", pr.Name, err)
		}
	}
	return dir
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
// LAST `pack use` of this pack contributed to cfg.MCP / cfg.KnowledgeBundles, so
// switching AWAY removes exactly that contribution — never a user's own
// manually-added entry. Git-ignored by default (runPackNew seeds a pack-local
// .gitignore line for it).
type packLock struct {
	MCP       []string `toml:"mcp,omitempty"`
	Knowledge []string `toml:"knowledge,omitempty"`
}

const packLockName = "pack.lock"

func packLockPath(root string) string { return filepath.Join(root, packLockName) }

// readPackLock reads root's pack.lock, best-effort: an absent or unparsable
// file returns the zero value (no recorded contribution — the caller's removal
// set is then empty, which is the safe default: never guess at what an older
// activation contributed).
func readPackLock(root string) packLock {
	var l packLock
	b, err := os.ReadFile(packLockPath(root))
	if err != nil {
		return l
	}
	_ = toml.Unmarshal(b, &l)
	return l
}

// writePackLock writes root's pack.lock (0644; not a secret — it holds server
// NAMES and canonical bundle PATHS, never a credential value).
func writePackLock(root string, l packLock) error {
	var buf bytes.Buffer
	if err := toml.NewEncoder(&buf).Encode(l); err != nil {
		return err
	}
	return os.WriteFile(packLockPath(root), buf.Bytes(), 0o644)
}

// prevPackKnowledgeIDs computes the canonical bundle ids the PREVIOUS active
// pack contributed, for removal on switch (F4). It prefers the recorded
// pack.lock; when that is empty (a pack activated before pack.lock existed, or
// one that predates F6's [[knowledge]] refs) it falls back to the v1 behavior
// of removing just the embedded knowledge/ dir, so an upgrade never leaves a
// stale v1 bundle stuck in cfg.KnowledgeBundles forever.
func prevPackKnowledgeIDs(prevRoot string, lock packLock) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		id = canonicalizeKnowledgeBundle(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range lock.Knowledge {
		add(id)
	}
	if len(lock.Knowledge) == 0 {
		if op, err := loadPack(prevRoot); err == nil && op.KnowledgeDir != "" {
			add(op.KnowledgeDir)
		}
	}
	return out
}

// resolvePackKnowledgeRef resolves one [[knowledge]] entry to an absolute local
// bundle path (F6). Shared=true TRAVELS: resolved via the existing
// resolveBundleRef, which clones/pulls a git URL into the shared knowledge
// cache (or uses a local path as-is) — an adopter who shares the pack pulls the
// SAME team bundle. Shared=false does NOT travel: it is deliberately NOT
// root-scoped (expandUser + Abs only) — pointing outside the pack at the
// owner's own machine is the entire point of a private reference; when the
// pack repo is shared, the reference line is simply inert for an adopter
// (nothing to resolve locally), and the referenced content never entered the
// pack's git tree.
func resolvePackKnowledgeRef(out io.Writer, k packKnowledge) (string, error) {
	source := strings.TrimSpace(k.Source)
	if source == "" {
		return "", fmt.Errorf("[[knowledge]] %q has no source", k.Name)
	}
	if k.Shared {
		return resolveBundleRef(source, knowledgeCacheDir(), out)
	}
	abs, err := filepath.Abs(expandUser(source))
	if err != nil {
		return "", fmt.Errorf("resolving private knowledge %q: %w", k.Name, err)
	}
	return abs, nil
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

// packRecreateLine is the ADR-3 "same breath" recreate instruction: any
// operation that changes the sandbox facet set (MCP attach, sandbox bin/
// wrappers) MUST print this, because --mcp/--kit are create-only — a running
// sandbox cannot pick either up without a recreate (packs.md §13 must-fix).
func printPackRecreateLine(out io.Writer) {
	fmt.Fprintln(out, "MCP attach + sandbox bin/ wrappers only take effect on a sandbox CREATE.")
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
		if cerr == nil && cfg.Pack == root {
			if cfg.AddMCP(name) {
				if err := cfg.Save(); err != nil {
					fmt.Fprintf(out, "note: saving config: %v\n", err)
				} else {
					if err := registerServers(cfg, env, out, []string{name}, findHostBinary); err != nil {
						fmt.Fprintf(out, "note: mcp registration: %v\n", err)
					}
					solicitPackCredentials(env, os.Stdin, out, isTTY(os.Stdin), p)
					lock := readPackLock(root)
					if !containsStr(lock.MCP, name) {
						lock.MCP = append(lock.MCP, name)
					}
					if err := writePackLock(root, lock); err != nil {
						fmt.Fprintf(out, "note: writing pack.lock: %v\n", err)
					}
					printPackRecreateLine(out)
				}
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
	var root string
	if isPackGitURL(arg) {
		r, err := clonePack(env, out, arg)
		if err != nil {
			fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
			os.Exit(1)
		}
		root = r
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
	var prevLock packLock
	if switching {
		prevLock = readPackLock(prevRoot)
	}

	// MCP set (F1 + ADR-1): remove exactly what the PREVIOUS pack's last
	// activation contributed (never a user's own manually-added MCP), then add
	// what the NEW pack declares. Reversible: pack-use(A) -> pack-use(B) ->
	// pack-use(A) restores cfg.MCP to what it was after the first pack-use(A).
	var removedMCP, addedMCP []string
	if switching {
		for _, m := range prevLock.MCP {
			if cfg.RemoveMCP(m) {
				removedMCP = append(removedMCP, m)
			}
		}
	}
	newMCP := packMcpNames(p)
	for _, m := range newMCP {
		if cfg.AddMCP(m) {
			addedMCP = append(addedMCP, m)
		}
	}

	// Knowledge (F4 + F6): remove exactly what the PREVIOUS pack contributed
	// (embedded dir + [[knowledge]] refs, from pack.lock — falling back to the
	// v1 embedded-dir-only removal when no lock was recorded yet), then add the
	// NEW pack's embedded dir + resolved [[knowledge]] refs (shared travels via
	// resolveBundleRef; private resolves to a local path that never entered the
	// pack's git tree).
	var removedKnowledge, addedKnowledge []string
	if switching {
		for _, id := range prevPackKnowledgeIDs(prevRoot, prevLock) {
			if cfg.RemoveKnowledgeBundle(id) {
				removedKnowledge = append(removedKnowledge, id)
			}
		}
	}
	var newKnowledgeIDs []string
	if p.KnowledgeDir != "" {
		if cfg.AddKnowledgeBundle(p.KnowledgeDir) {
			addedKnowledge = append(addedKnowledge, p.KnowledgeDir)
		}
		newKnowledgeIDs = append(newKnowledgeIDs, canonicalizeKnowledgeBundle(p.KnowledgeDir))
		cfg.AddService("knowledge")
	}
	for _, k := range p.Manifest.Knowledge {
		resolved, rerr := resolvePackKnowledgeRef(out, k)
		if rerr != nil {
			fmt.Fprintf(out, "note: knowledge %q: %v (skipping)\n", k.Name, rerr)
			continue
		}
		if cfg.AddKnowledgeBundle(resolved) {
			addedKnowledge = append(addedKnowledge, resolved)
		}
		newKnowledgeIDs = append(newKnowledgeIDs, canonicalizeKnowledgeBundle(resolved))
		cfg.AddService("knowledge")
	}

	// Config layering (F4): a value the pack declares overwrites; an undeclared
	// one is left as whatever was already configured ("layered when active", per
	// the schema doc — not a reset-to-zero on every switch).
	if p.Manifest.GogAccount != "" {
		cfg.SetGogAccount(p.Manifest.GogAccount)
	}
	if m := strings.TrimSpace(p.Manifest.OllamaBridgeModel); m != "" {
		cfg.OllamaBridgeModel = m
	}

	cfg.Pack = root

	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "pi-stack pack use: saving config: %v\n", err)
		os.Exit(1)
	}

	// --- post-Save: best-effort side effects (each already idempotent). ---

	fmt.Fprintf(out, "active pack -> %s\n", root)
	if len(removedMCP) > 0 {
		fmt.Fprintf(out, "detached mcp (previous pack): %s\n", strings.Join(removedMCP, ", "))
	}
	if len(addedMCP) > 0 {
		fmt.Fprintf(out, "attached mcp: %s\n", strings.Join(addedMCP, ", "))
		if err := registerServers(cfg, env, out, addedMCP, findHostBinary); err != nil {
			fmt.Fprintf(out, "note: mcp registration: %v\n", err)
		}
	}
	for _, id := range removedKnowledge {
		fmt.Fprintf(out, "knowledge bundle detached (previous pack): %s\n", id)
	}
	for _, id := range addedKnowledge {
		fmt.Fprintf(out, "knowledge bundle registered: %s\n", id)
	}

	// Solicit any 1Password creds this pack's reference-only integrations need.
	solicitPackCredentials(env, os.Stdin, out, isTTY(os.Stdin), p)

	// Record this activation's contribution for the NEXT switch's removal set.
	if err := writePackLock(root, packLock{MCP: newMCP, Knowledge: newKnowledgeIDs}); err != nil {
		fmt.Fprintf(out, "note: writing pack.lock: %v\n", err)
	}

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
	cfg.Pack = ""
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "pi-stack pack rm: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "detached active pack (%s). The files are untouched; re-attach with `pi-stack pack use`.\n", old)
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
	return dest, nil
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
