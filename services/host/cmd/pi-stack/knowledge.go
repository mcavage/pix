package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"pi-stack/host/config"
)

// runKnowledge is the `knowledge` verb tree: `init`, `use`, `ls`. It scaffolds
// and wires the GLOBAL OKF knowledge bundle the knowledge service (:11436)
// indexes, so nobody hand-edits config.toml or hand-authors an OKF skeleton.
//
//	pi-stack knowledge init [DIR]     scaffold a spec-correct OKF bundle (default
//	                                  <config-dir>/knowledge), git init it, and
//	                                  wire it into config (services += knowledge,
//	                                  knowledge_bundles += DIR). Idempotent.
//	pi-stack knowledge use <path|url> point the global KB at an existing bundle: a
//	                                  local path indexed in place, a git URL
//	                                  cloned/pulled to <config-dir>/knowledge-cache.
//	pi-stack knowledge ls             list configured bundles + daemon health.
func runKnowledge(argv []string) {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pi-stack knowledge <init|use|ls> [args]")
		os.Exit(2)
	}
	switch argv[0] {
	case "init":
		runKnowledgeInit(argv[1:])
	case "use":
		runKnowledgeUse(argv[1:])
	case "ls":
		runKnowledgeLs()
	default:
		fmt.Fprintf(os.Stderr, "pi-stack knowledge: unknown subcommand %q (want: init, use, ls)\n", argv[0])
		os.Exit(2)
	}
}

// defaultKnowledgeDir is <config-dir>/knowledge — the sibling of config.toml,
// resolved the same way config.Path() resolves its directory.
func defaultKnowledgeDir() string {
	return filepath.Join(filepath.Dir(config.Path()), "knowledge")
}

// knowledgeCacheDir is <config-dir>/knowledge-cache — where git-URL bundles are
// cloned/pulled so the resolved local path is what gets indexed and scoped.
func knowledgeCacheDir() string {
	return filepath.Join(filepath.Dir(config.Path()), "knowledge-cache")
}

// runKnowledgeInit is the CLI entry point for `knowledge init [DIR]`.
func runKnowledgeInit(argv []string) {
	dir := defaultKnowledgeDir()
	if len(argv) > 0 && argv[0] != "" {
		abs, err := filepath.Abs(argv[0])
		if err != nil {
			fmt.Fprintf(os.Stderr, "pi-stack knowledge init: %v\n", err)
			os.Exit(1)
		}
		dir = abs
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack knowledge init: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := knowledgeInit(cfg, dir, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack knowledge init: %v\n", err)
		os.Exit(1)
	}
}

// knowledgeInit scaffolds a spec-correct OKF bundle at dir (idempotent: it never
// clobbers an existing bundle, only fills in missing files), git-inits it when
// new, then wires it into cfg (knowledge_bundles += dir, services += knowledge)
// and Save()s. Testable: takes cfg + out and returns an error instead of exiting.
func knowledgeInit(cfg *config.Config, dir string, out io.Writer) error {
	existed := isOKFBundle(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	// git init a fresh bundle so it is version-controllable from the start.
	// Best-effort: a missing git is a warning, not a failure — the scaffold +
	// config wiring are still useful without it.
	if !isGitRepo(dir) {
		if _, err := exec.LookPath("git"); err != nil {
			fmt.Fprintf(out, "note: git not found on PATH — skipping git init (install git to version this bundle)\n")
		} else if err := gitInit(dir); err != nil {
			fmt.Fprintf(out, "note: git init failed (%v) — bundle scaffolded but not version-controlled\n", err)
		}
	}

	if existed {
		fmt.Fprintf(out, "Bundle already present at %s — not clobbering, re-wiring config.\n", dir)
	} else if err := scaffoldBundle(dir); err != nil {
		return err
	} else {
		fmt.Fprintf(out, "Scaffolded OKF bundle at %s\n", dir)
	}

	wireKnowledge(cfg, dir)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintf(out, "Wired into %s (knowledge_bundles += %s, services += knowledge)\n", config.Path(), dir)
	fmt.Fprintln(out, "Next: `pi-stack serve` indexes it; recall hits the knowledge service on :11436.")
	return nil
}

// runKnowledgeUse is the CLI entry point for `knowledge use <path|url>`.
func runKnowledgeUse(argv []string) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		fmt.Fprintln(os.Stderr, "usage: pi-stack knowledge use <path|git-url>")
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack knowledge use: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := knowledgeUse(cfg, argv[0], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack knowledge use: %v\n", err)
		os.Exit(1)
	}
}

// knowledgeUse points the global KB at an existing bundle: a local path is used
// in place; a git URL is cloned (or pulled if already cached) into
// <config-dir>/knowledge-cache/<repo>. Either way the resolved local path is
// added to knowledge_bundles and the knowledge service is enabled.
func knowledgeUse(cfg *config.Config, ref string, out io.Writer) error {
	resolved, err := resolveBundleRef(ref, knowledgeCacheDir(), out)
	if err != nil {
		return err
	}
	wireKnowledge(cfg, resolved)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintf(out, "Using bundle %s\n", resolved)
	fmt.Fprintf(out, "Wired into %s (knowledge_bundles = %v, services += knowledge)\n", config.Path(), cfg.KnowledgeBundles)
	fmt.Fprintln(out, "Next: `pi-stack serve` indexes it; recall hits the knowledge service on :11436.")
	return nil
}

// resolveBundleRef turns a bundle reference into an absolute local path. A local
// path resolves to its absolute form; a git URL is cloned into cacheDir/<repo>
// (or pulled if already present). Cloning requires git — its absence is a clear
// error on this path (unlike init, where git is optional).
func resolveBundleRef(ref, cacheDir string, out io.Writer) (string, error) {
	ref = strings.TrimSpace(ref)
	if !isGitURL(ref) {
		abs, err := filepath.Abs(ref)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", ref, err)
		}
		return abs, nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git not found on PATH — needed to clone %s; install git", ref)
	}
	dest := filepath.Join(cacheDir, repoSlug(ref))
	if isGitRepo(dest) {
		fmt.Fprintf(out, "Updating cached bundle %s\n", dest)
		if err := gitPull(dest); err != nil {
			return "", fmt.Errorf("pulling %s: %w", dest, err)
		}
		return dest, nil
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache dir %s: %w", cacheDir, err)
	}
	fmt.Fprintf(out, "Cloning %s into %s\n", ref, dest)
	if err := gitClone(ref, dest); err != nil {
		return "", fmt.Errorf("cloning %s: %w", ref, err)
	}
	return dest, nil
}

// runKnowledgeLs is the CLI entry point for `knowledge ls`.
func runKnowledgeLs() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pi-stack knowledge ls: loading config: %v\n", err)
		os.Exit(1)
	}
	knowledgeLs(cfg, defaultShellEnv(), os.Stdout)
}

// knowledgeLs prints the configured knowledge bundles and whether the knowledge
// service is reachable on :11436. It degrades cleanly when the daemon is down.
func knowledgeLs(cfg *config.Config, env shellEnv, out io.Writer) {
	fmt.Fprintf(out, "# config: %s\n", config.Path())
	if len(cfg.KnowledgeBundles) == 0 {
		fmt.Fprintln(out, "knowledge_bundles: (none) — run `pi-stack knowledge init` to create one")
	} else {
		fmt.Fprintln(out, "knowledge_bundles:")
		for _, b := range cfg.KnowledgeBundles {
			fmt.Fprintf(out, "  - %s\n", b)
		}
	}
	enabled := false
	for _, s := range cfg.Services {
		if s == "knowledge" {
			enabled = true
			break
		}
	}
	if !enabled {
		fmt.Fprintln(out, "service: disabled — enable with `pi-stack config set services knowledge`")
	} else if env.dial != nil && env.dial(11436) {
		fmt.Fprintln(out, "service: up (:11436)")
	} else {
		fmt.Fprintln(out, "service: down (:11436 unreachable) — start it with `pi-stack serve`")
	}
}

// wireKnowledge adds the bundle dir to knowledge_bundles and ensures the
// knowledge service is enabled. Both are idempotent.
func wireKnowledge(cfg *config.Config, dir string) {
	cfg.AddKnowledgeBundle(dir)
	cfg.AddService("knowledge")
}

// isGitURL reports whether ref should be treated as a git URL (cloneable) rather
// than a local path. Pure and side-effect free so it is unit-testable without
// touching the network or disk.
func isGitURL(ref string) bool {
	ref = strings.TrimSpace(ref)
	switch {
	case strings.HasPrefix(ref, "http://"),
		strings.HasPrefix(ref, "https://"),
		strings.HasPrefix(ref, "git://"),
		strings.HasPrefix(ref, "ssh://"),
		strings.HasPrefix(ref, "git@"):
		return true
	case strings.HasSuffix(ref, ".git"):
		return true
	}
	return false
}

// repoSlug derives a filesystem-safe cache dir name from a git URL: the last
// path segment with any trailing ".git" and separators stripped.
func repoSlug(url string) string {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	if s == "" {
		return "bundle"
	}
	return s
}

// isOKFBundle reports whether dir already looks like an OKF bundle (has a
// root index.md), so init knows not to clobber it.
func isOKFBundle(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, "index.md"))
	return err == nil && !fi.IsDir()
}

// isGitRepo reports whether dir has a .git directory.
func isGitRepo(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil && fi.IsDir()
}

func gitInit(dir string) error {
	return exec.Command("git", "-C", dir, "init", "-q").Run()
}

func gitClone(url, dest string) error {
	return exec.Command("git", "clone", "-q", url, dest).Run()
}

func gitPull(dir string) error {
	return exec.Command("git", "-C", dir, "pull", "-q", "--ff-only").Run()
}

// scaffoldBundle writes a spec-correct OKF skeleton into dir. It writes each
// file only when absent so re-running never clobbers hand-authored content.
//
// OKF reserved-file rules (see services/host/okf): the bundle-root index.md is
// the ONLY reserved file that may carry frontmatter, and only `okf_version`;
// log.md carries NO frontmatter (date-grouped entries); non-reserved concept
// files carry a REQUIRED `type` frontmatter key.
func scaffoldBundle(dir string) error {
	files := map[string]string{
		"index.md":                     indexScaffold,
		"log.md":                       logScaffold,
		"reference/getting-started.md": conceptScaffold,
	}
	for rel, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if _, err := os.Stat(full); err == nil {
			continue // don't clobber an existing file
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", rel, err)
		}
	}
	return nil
}

// indexScaffold is the bundle-root index.md: the only reserved file that may
// carry frontmatter, and only okf_version. The body is a plain markdown listing.
const indexScaffold = `---
okf_version: "0.1"
---
# Knowledge Bundle

An OKF (Open Knowledge Format) bundle indexed by the pi-stack knowledge service.
Each concept is a markdown file with a required ` + "`type`" + ` frontmatter key.
This root index is a human-readable listing; it is not itself a concept.

## Concepts

- [Getting started](reference/getting-started.md)
`

// logScaffold is the reserved log.md: date-grouped entries, NO frontmatter.
const logScaffold = `# Log

Date-grouped notes about changes to this bundle. No frontmatter here (log.md is
a reserved OKF file).

## 2024-01-01

- Bundle created by ` + "`pi-stack knowledge init`" + `.
`

// conceptScaffold is a starter concept with the REQUIRED type frontmatter and a
// # Citations section.
const conceptScaffold = `---
type: reference
title: Getting started
description: How this knowledge bundle works and how to add to it.
---
# Getting started

Add one markdown file per concept. Every concept file needs a ` + "`type`" + `
frontmatter key (e.g. reference, guide, dataset, table). Optional keys: title,
description, resource, tags, timestamp.

Link between concepts with relative markdown links, e.g. [the index](/index.md).
List sources under a ` + "`# Citations`" + ` heading.

# Citations
- https://github.com/mcavage/pi-stack
`
