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
	"bytes"
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
	Name              string `toml:"name"`
	Schema            int    `toml:"schema"`
	OllamaBridgeModel string `toml:"ollama_bridge_model,omitempty"`
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
		p.SkillsDir = d
	}
	if d := filepath.Join(root, "knowledge"); dirHasEntries(d) {
		p.KnowledgeDir = d
	}
	return p, nil
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
		runPackUse(os.Stdout, rest)
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

// runPackNew adopts a pre-existing repo (or one already carrying pack.toml) in
// place, else creates + git-inits a fresh pack. Never re-inits or clobbers.
func runPackNew(env shellEnv, out io.Writer, rest []string) {
	root := packTarget(rest)
	// Already a pack? Nothing to do.
	if _, err := os.Stat(filepath.Join(root, packManifestName)); err == nil {
		fmt.Fprintf(out, "already a pack: %s\n", root)
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
	fmt.Fprintf(out, "use it:  pi-stack pack use %s\n", root)
}

// runPackAdd writes one artifact into a pack (implicit-create), then registers
// it by presence (skills/knowledge are discovered by convention).
func runPackAdd(env shellEnv, out io.Writer, rest []string) {
	if len(rest) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pi-stack pack add <skill|knowledge> <name> [PACK]")
		os.Exit(2)
	}
	kind, name := rest[0], rest[1]
	root := personalPackRoot()
	if len(rest) >= 3 {
		root = expandUser(rest[2])
	}
	// Implicit-create the pack if absent.
	if _, err := os.Stat(filepath.Join(root, packManifestName)); err != nil {
		runPackNew(env, out, []string{root})
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
		fmt.Fprintln(out, "no active pack (`pi-stack pack use <path>` or `pi-stack pack new`)")
		return
	}
	p, err := loadPack(active)
	if err != nil {
		fmt.Fprintf(out, "active pack %s: %v\n", active, err)
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
}

func runPackUse(out io.Writer, rest []string) {
	if len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: pi-stack pack use <path>")
		os.Exit(2)
	}
	root := expandUser(rest[0])
	abs, err := filepath.Abs(root)
	if err == nil {
		root = abs
	}
	if _, err := loadPack(root); err != nil {
		fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
		os.Exit(1)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(out, "pi-stack pack use: %v\n", err)
		os.Exit(1)
	}
	cfg.Pack = root
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(out, "pi-stack pack use: saving config: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(out, "active pack -> %s\n", root)
}

func runPackRm(out io.Writer, _ []string) {
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
  use <path>              set the active pack (run mounts its skills + knowledge)
  rm                      detach the active pack (files untouched)
`
