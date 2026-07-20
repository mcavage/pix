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

// packManifest is pack.toml. Minimal in v1: identity + optional model prefs.
// Skills and knowledge are discovered by convention (skills/, knowledge/), so a
// pack does not have to enumerate them.
type packManifest struct {
	Name              string            `toml:"name"`
	Schema            int               `toml:"schema"`
	OllamaBridgeModel string            `toml:"ollama_bridge_model,omitempty"`
	Integrations      []packIntegration `toml:"integrations,omitempty"`
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
	return p, nil
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

// applyPackToLaunch mounts the active pack into a launch (run OR task): it appends
// the pack's skills dir to o.Skills and applies the pack's ollama model pref, and
// warns about declared integrations (a pack NEVER auto-enables a host MCP — that
// would be a silent host-exec vector — and a missing credential is surfaced).
// Knowledge is NOT handled here: a persisted active pack already has its bundle
// in cfg.KnowledgeBundles (added at `pack use` time, indexed by the daemon), and
// a transient --pack override's knowledge is deliberately not scoped (the daemon
// wouldn't have indexed it). An EXPLICIT --pack that fails to load is fatal; the
// personal/active fallback failing is a silent skip.
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
	for _, ig := range p.Manifest.Integrations {
		if ig.MCP != "" && !containsStr(cfg.MCP, ig.MCP) {
			fmt.Fprintf(os.Stderr, "pi-stack: pack uses MCP %q — enable it explicitly: pi-stack config set mcp %s\n", ig.MCP, ig.MCP)
		}
		if ig.Env != "" && !opRefFilled(env, ig.Env) {
			fmt.Fprintf(os.Stderr, "pi-stack: pack integration %q needs a credential — set it: pi-stack secret set %s op://vault/item/field\n", ig.Name, ig.Env)
		}
	}
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
		fmt.Fprintln(os.Stderr, "usage: pi-stack pack add <skill|knowledge> <name> [PACK]")
		os.Exit(2)
	}
	kind, name := rest[0], rest[1]
	if !safeArtifactName(name) {
		fmt.Fprintf(os.Stderr, "pi-stack pack add: invalid name %q (letters, digits, -, _, . only; no path separators)\n", name)
		os.Exit(2)
	}
	root := personalPackRoot()
	if len(rest) >= 3 {
		root = expandUser(rest[2])
	}
	// Implicit-create the pack if absent.
	if _, err := os.Stat(filepath.Join(root, packManifestName)); err != nil {
		runPackNew(env, out, []string{root})
	}
	// Refuse to write through a symlinked skills//knowledge/ dir (an adopted pack
	// could point it outside the pack root); same posture as loadPack's mount check.
	if isSymlinkPath(filepath.Join(root, "skills")) || isSymlinkPath(filepath.Join(root, "knowledge")) {
		fmt.Fprintf(os.Stderr, "pi-stack pack add: %s has a symlinked skills//knowledge/ dir; refusing to write through it\n", root)
		os.Exit(1)
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
	default:
		fmt.Fprintf(os.Stderr, "pi-stack pack add: unknown kind %q (want: skill, knowledge)\n", kind)
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
	// Drop the PREVIOUS active pack's knowledge bundle so switching packs doesn't
	// accumulate stale bundles into every later context (single-active-pack
	// isolation). Best-effort: only removes a bundle that came from that pack.
	if cfg.Pack != "" && cfg.Pack != root {
		if oldp, oerr := loadPack(cfg.Pack); oerr == nil && oldp.KnowledgeDir != "" {
			cfg.RemoveKnowledgeBundle(oldp.KnowledgeDir)
		}
	}
	cfg.Pack = root
	// Persist the pack's knowledge bundle into config (+ enable the knowledge
	// service) so `serve` actually INDEXES it — a per-run in-memory append would be
	// invisible to the daemon (which reloads config from disk). Idempotent.
	if p.KnowledgeDir != "" {
		if cfg.AddKnowledgeBundle(p.KnowledgeDir) {
			fmt.Fprintf(out, "knowledge bundle registered: %s\n", p.KnowledgeDir)
		}
		if cfg.AddService("knowledge") {
			fmt.Fprintln(out, "enabled the knowledge service")
		}
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "pi-stack pack use: saving config: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "active pack -> %s\n", root)
	// Solicit any 1Password creds this pack's reference-only integrations need.
	solicitPackCredentials(env, os.Stdin, out, isTTY(os.Stdin), p)
	// A knowledge change is daemon-affecting: restart/advise the running serve so
	// the new bundle is indexed (mirrors `knowledge use`). Best-effort.
	propagateServeConfig(defaultServeReloader(), out)
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

A pack is a git-backed bundle of skills + knowledge (+ later mcp/proxies/config)
that defines your context. See docs/design/packs.md.

  new [PATH]              adopt an existing repo (or one with pack.toml) as a
                          pack, else git-init a fresh one. Default PATH is the
                          personal pack root (~/.local/share/pi-stack/skills).
  add <kind> <name> [P]   add a skill|knowledge doc to pack P (default: personal;
                          implicit-creates the pack)
  ls                      show the active pack
  show [PATH]             inspect a pack (default: the active pack)
  use <path|git-url>      set the active pack; a git URL is cloned to
                          ~/.local/share/pi-stack/packs/<name> (optional #ref pin)
  rm                      detach the active pack (files untouched)
`
