package main

// arch_test.go enforces docs/design/architecture.md. It is the whole reason
// that document is a contract rather than aspirational prose: a layering rule
// nothing checks is a layering rule that lasted until the next deadline.
//
// The rule: imports point DOWN, never up, and never sideways within L1.
//
//	L4  cmd/pix        argv -> a command; owns os.Exit
//	L3  workflow/*     orchestrate L1+L2
//	L2  health         L1 probes -> a Snapshot
//	L1  capability/*   one domain each; MAY NOT import each other
//	L0  foundation     sys, config, routing, rpc, cli, hostenv
//
// The sideways ban on L1 is the load-bearing clause. The 40,905-line package
// this architecture replaces was a web precisely because capabilities called
// each other: `slack` exported 3 symbols and needed 27 back, spanning readiness
// types, secret's op-ref parsing and MCP registration. Workflows compose
// capabilities; capabilities stay leaves. If a capability needs something from
// a sibling, the caller passes it in.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// layer numbers. Higher may import lower; equal may not import equal EXCEPT
// where noted (L0 is internally ordered by hand, see l0Order).
const (
	layerFoundation = 0
	layerCapability = 1
	layerReadiness  = 2
	layerWorkflow   = 3
	layerCommand    = 4
)

// pkgLayer maps a package path (relative to services/host) to its layer.
// A package absent from this map is UNPLACED and fails the test: a new package
// must state where it belongs, because "wherever it ended up" is how the last
// architecture was chosen.
var pkgLayer = map[string]int{
	// L0 — foundation. No domain knowledge.
	"sys": layerFoundation,
	// sys/systest is placed and layer-checked like any other L0 package, but its
	// status is honestly test-only: its every importer, across the whole module,
	// is a _test.go file (grep it) — sys.System's one fake, consumed only to
	// build fixtures. It cannot be reclassified as test-only support the way
	// cmd/pix/corpus was (U11k): its file must keep the plain .go name, because a
	// package built ONLY from _test.go files cannot be imported by another
	// package's tests. So it stays production-shaped by necessity while being
	// test-only in fact; this comment is that fact, not a claim it has a real
	// caller.
	"sys/systest": layerFoundation,
	"config":      layerFoundation,
	"routing":     layerFoundation,
	"rpc":         layerFoundation,
	"cli":         layerFoundation,
	// hostenv, unlike sys/systest, is genuinely production L0: pack, provision,
	// launch, doctor, models, secret, inference and mcp all import it from
	// non-test files (hostenv.Env is the injected "what can this host do" seam
	// those workflows and capabilities take as a parameter).
	"hostenv":  layerFoundation,
	"launcher": layerFoundation,
	// lease is U04a's per-sandbox lifecycle/ref-lock primitives (an immutable
	// creation record, an identity-bound keep marker, an flock-backed
	// reference lock): unix syscalls + stdlib only, no domain knowledge, same
	// tier as sys/config/workspace.
	"lease": layerFoundation,
	// workspace is L0, not a capability: it answers "where am I working and what
	// state is stored there", which is location resolution in the same family as
	// config. It imports only config and sys. Filing it as a capability was a
	// mis-read that made two legitimate users (memory, knowledge) look like
	// sibling violations.
	"workspace": layerFoundation,
	// unitreport is the SERIALIZED supervision snapshot and nothing else: no
	// process, no lifecycle, no imports outside the stdlib. It is L0 because
	// supervise (L2) writes it while service (L1) and workflow/doctor (L3) read
	// it — a shape shared by three layers cannot live in any one of them.
	"unitreport": layerFoundation,
	// unitreport/unitreporttest is placed and layer-checked like sys/systest: a
	// shared table of supervision-snapshot scenarios that `service` and
	// `workflow/doctor` both classify (see its doc comment). Its every importer
	// is a _test.go file; it stays production-shaped for the same reason
	// sys/systest does — a package built only from _test.go files cannot be
	// imported by another package's tests.
	"unitreport/unitreporttest": layerFoundation,

	// L1 — capability. One domain each, siblings invisible to each other.
	//
	// okf and knowledge (the built-in OKF knowledge service, :11436) are
	// deliberately ABSENT from this map: W2/U03A deleted the package outright,
	// not merely its callers. A placement entry for a package that no longer
	// exists on disk is a GHOST — see TestArchitecture_NoGhostPlacements below,
	// which fails the day one lingers instead of relying on someone noticing.
	"inference": layerCapability,
	"plugin":    layerCapability,
	"service":   layerCapability,
	"memory":    layerCapability,
	"secret":    layerCapability,
	"mcp":       layerCapability,
	// sandbox is U04b's focused L1 sandbox domain: naming, the tolerant sbx-
	// listing parser, create-vs-exec argv planning, fingerprint comparison and
	// non-force removal planning. Pure and dependency-free (see sandbox/doc.go),
	// same tier as its capability siblings, invisible to them.
	"sandbox": layerCapability,
	// packinfo is the READ-ONLY pack model — pack.toml's schema, the fail-closed
	// loader, active-root resolution and the facts derived from them. It exists
	// because launch, doctor and provision all need "what pack is active and what
	// does it declare" and NONE of them may import workflow/pack to ask: that was
	// the L3-to-L3 web this file now forbids outright. Trust and adoption stay at
	// L3; this package reads, validates, and decides nothing.
	"packinfo": layerCapability,
	// workflow/task is Story06's L1 task-checkout capability: naming, metadata,
	// the clone/worktree mechanism, and the git-hygiene removal guard, with NO
	// import of workflow/launch, any sandbox runner, or lease (see its doc
	// comment). It lives under workflow/ for discoverability next to the CLI
	// surface it backs, but it is a leaf capability, not a workflow: it composes
	// nothing below it and orchestrates no other capability.
	"workflow/task": layerCapability,

	// supervise sits ABOVE the capabilities on purpose: it is the process
	// lifecycle that RUNS one (plugin), not a domain of its own. Filing it at L1
	// would make its plugin import a sideways call; filing it at L2 states the
	// truth — it composes the plugin capability into supervised units, and only
	// the daemon entry point (package main) consumes it.
	"supervise": layerReadiness,

	// L2 — the shared model of "is this working".
	//
	// The readiness/axis pair that used to live here is GONE (W5/U11r): a
	// Requirement × Verdict matrix, a lazy axis registry, four exit codes and two
	// renderers, split across a model package and a builders package nobody could
	// reason about separately. health replaced it — Probe -> Result -> Snapshot,
	// with the probes in the same package — and the utilities that lived in axis
	// only by history went to the domains that own them (model resolution,
	// endpoint resolution and machine sizing to inference; "is a model key
	// present" to secret; the launch gate and its warnings to workflow/launch).
	"health": layerReadiness,

	// L3 — workflow. A user-facing verb's logic. Allowed to compose L1+L2;
	// may not contain a capability.
	//
	// workflow/backup, workflow/man, and workflow/upgrade (the launcher-side
	// `pix backup`/`pix restore`, `pix man`/`--man`, and `pix upgrade` verbs) were
	// deleted along with their dead code once U01 retired all three surfaces and
	// left nothing importing them (see retired.go). The pix-host-side `backup`/
	// `restore` subcommands they pointed at collapsed further in U07b: the
	// multi-component archive is gone, and what survives is `pix-host memory
	// snapshot|restore` at root (memory_snapshot.go) — one sqlite file, not an
	// archive format, and not this launcher package.
	//
	// workflow/reset (the launcher-side `pix reset`/`pix state reset` move-
	// things-aside verb, 687 lines) is GONE too (U11r): ephemeral sandboxes plus
	// setup/doctor do its job now, and recovering from a broken host is a manual,
	// evidence-first walk (`pix doctor` -> `pix config path`/`pix status --json`
	// -> `pix setup`), never an automated wipe. Both surfaces answer with the
	// standard PIX_RETIRED notice (retired.go); nothing imports the package.
	//
	// slack (the OAuth/credential/MCP-registration workflow, plus the slackoauth
	// L1 capability underneath it) was externalized in W2/U02a — see
	// docs/design/slack-setup.md — and neither package exists in the public tree.
	//
	// pack is a workflow for the same reason slack was: `pix pack use` resolves
	// a pack, then registers its MCP servers, wires its knowledge refs, seeds
	// its credentials and restarts services. It consumes five capabilities and
	// nothing below L4 consumes it. The capability-shaped parts inside it
	// (manifest parsing, the trust store, the host BoM) are candidates to split
	// out later; being a workflow is already correct today.
	//
	// gworkspace (the built-in `pix gworkspace setup|status|disable` wizard and
	// its headless-spawn probing) was externalized the same way in W2/U02B — see
	// docs/design/gworkspace-externalization.md — so that package is gone too;
	// gog is registered generically through `pix mcp register`.
	"workflow/pack":   layerWorkflow,
	"workflow/doctor": layerWorkflow,
	"workflow/launch": layerWorkflow,
	// workflow/models is `pix models add`: the inference selection, live
	// verification and roster machinery that used to be welded into setup's
	// keys/inference mutation steps. It is a workflow (it composes config,
	// secret, inference and routing), and it is the ONE place a provider
	// credential is solicited now that setup only probes for one.
	"workflow/models": layerWorkflow,
	// provision is `pix setup` itself: check, apply, check again. It composes
	// health (L2) and the applies for the three things setup installs, and owns
	// no domain knowledge of its own.
	"workflow/provision": layerWorkflow,

	// L4 — the command layer.
	"cmd/pix": layerCommand,
	// cmd/pix/corpus (the W0 U00b golden CLI corpus + retirement-manifest
	// harness) is deliberately ABSENT from this map: U11k reclassified it as
	// test-only support (every file under it is now a _test.go file, so it
	// has zero production LOC and no runtime caller). scanPackages below
	// skips packages with no production .go files for exactly this reason —
	// a package that is entirely tests has no layer to place, the same way it
	// has no budget to track in scripts/arch-metrics/budgets.json.

	// "." is the pix-host daemon binary's own root package — a separate program
	// from cmd/pix, but the same shape: argv/RPC in, dispatch out. It was
	// exempted (-1) as a placeholder; it earns L4 honestly — its production
	// imports (config, cli, routing, inference, plugin, supervise,
	// workflow/pack) are all L0-L3, so it satisfies the down-only rule without
	// help. Its examples/ tree is gone with the MCP plugin transport it
	// demonstrated (U11j).
	".": layerCommand,
}

// l0Order breaks ties inside L0, which is the one layer with internal
// structure: sys and config are the true bottom, and cli/hostenv are allowed to
// build on them. Without this, "equal layers may not import each other" would
// forbid cli from importing sys, which is nonsense.
// Ranks, lowest first: config and routing are pure file formats with no
// dependencies at all; sys sits above them because sys.Real.StateDir delegates
// to config, which is correct — the OS seam should not re-derive the launcher's
// data layout.
var l0Order = map[string]int{
	"config": 0, "routing": 0, "unitreport": 0,
	"sys": 1, "rpc": 1, "launcher": 1,
	"workspace":   2,
	"sys/systest": 2, "hostenv": 3,
	"cli": 4,
	// unitreporttest imports only unitreport (rank 0), so any rank above that
	// satisfies the strictly-lower-rank rule; parked alongside cli.
	"unitreport/unitreporttest": 4,
}

// drainingPackages was the ONLY exemption, and it is now EMPTY: cmd/pix was
// the single entry, and it satisfies the layering without help. Every package
// in the module obeys the rule.
//
// The mechanism stays because it is what made the rule adoptable — the
// alternative was enforcing nothing until the 40,905-line package was finished,
// which is how layering rules become aspirational prose. Adding an entry is
// legitimate for a package genuinely mid-extraction; it is not a place to put a
// violation you do not want to fix, and TestArchitecture_DrainingListIsShrinking
// is what keeps that true.
var drainingPackages = map[string]bool{}

func TestArchitecture_ImportsPointDown(t *testing.T) {
	pkgs := scanPackages(t)

	var unplaced []string
	for p := range pkgs {
		if _, ok := pkgLayer[p]; !ok {
			unplaced = append(unplaced, p)
		}
	}
	sort.Strings(unplaced)
	if len(unplaced) > 0 {
		t.Fatalf("these packages are not placed in a layer: %v\n"+
			"Add them to pkgLayer in this file. A new package must declare where it belongs — "+
			"\"wherever it ended up\" is how the previous architecture was chosen.", unplaced)
	}

	checkImports(t, pkgs)
}

// checkImports is the rule itself, separated from the scan so a derived case can
// run it over a constructed graph. t is an argument, not a receiver, precisely so
// TestArchitecture_SiblingWorkflowRuleIsEnforced can hand it a throwaway *testing.T
// and assert that it FAILS.
func checkImports(t *testing.T, pkgs map[string][]string) {
	t.Helper()
	for pkg, imports := range pkgs {
		from, ok := pkgLayer[pkg]
		if !ok || from < 0 || drainingPackages[pkg] {
			continue
		}
		for _, imp := range imports {
			to, ok := pkgLayer[imp]
			if !ok || to < 0 {
				continue
			}
			// A sub-package may import its parent: monitor/tui -> monitor is a
			// child consuming the thing it renders, not two siblings reaching
			// across a boundary. The parent must not import the child, which the
			// down-only rule already forbids.
			if strings.HasPrefix(pkg, imp+"/") {
				continue
			}
			switch {
			case to > from:
				t.Errorf("LAYER VIOLATION: %s (L%d) imports %s (L%d).\n"+
					"  Imports point down. Move the shared thing lower, or invert the call so the "+
					"higher layer passes what the lower one needs.", pkg, from, imp, to)
			case to == from && from == layerWorkflow:
				t.Errorf("SIBLING WORKFLOW VIOLATION: workflow %s imports workflow %s.\n"+
					"  L3 workflows may not import each other either: a verb that needs another "+
					"verb's authority is a composition, and composition belongs to L4. Move the "+
					"shared FACTS down to a capability and let cmd/pix pass the rest in.", pkg, imp)
			case to == from && from == layerCapability:
				t.Errorf("SIBLING VIOLATION: capability %s imports capability %s.\n"+
					"  L1 capabilities may not import each other — that is the clause that keeps this "+
					"a DAG instead of the web it replaced. Let the workflow call both and pass the "+
					"result down.", pkg, imp)
			case to == from && from == layerFoundation:
				if l0Order[imp] >= l0Order[pkg] {
					t.Errorf("FOUNDATION ORDER: %s (rank %d) imports %s (rank %d).\n"+
						"  Within L0, imports must go to a strictly lower rank; see l0Order.",
						pkg, l0Order[pkg], imp, l0Order[imp])
				}
			}
		}
	}
}

// TestArchitecture_SiblingWorkflowRuleIsEnforced proves the L3-to-L3 clause is a
// RULE and not a comment, by deriving a violation from the real import graph
// instead of hand-writing a fixture: it takes two workflows that actually exist,
// pretends one imports the other, and requires the checker to say so. A guard
// that only ever sees passing input is a guard nobody has seen work — this file
// documents six in this repo that rotted exactly that way.
func TestArchitecture_SiblingWorkflowRuleIsEnforced(t *testing.T) {
	var workflows []string
	for pkg, layer := range pkgLayer {
		if layer == layerWorkflow {
			workflows = append(workflows, pkg)
		}
	}
	sort.Strings(workflows)
	if len(workflows) < 2 {
		t.Fatalf("expected at least two L3 workflows to derive the case from, got %v", workflows)
	}
	from, to := workflows[0], workflows[1]
	fake := &testing.T{}
	checkImports(fake, map[string][]string{from: {to}})
	if !fake.Failed() {
		t.Errorf("the layering checker accepted %s importing %s; L3 workflows are siblings and "+
			"the rule that forbids it is the whole reason provision no longer calls launch", from, to)
	}
	// The same pair with the import REMOVED must pass, so the case above proves
	// the edge is what failed and not the fixture.
	clean := &testing.T{}
	checkImports(clean, map[string][]string{from: nil, to: nil})
	if clean.Failed() {
		t.Errorf("two workflows importing nothing must satisfy the rule; the check is over-broad")
	}
}

// TestArchitecture_NoGhostPlacements is the inverse of the unplaced-package
// check in TestArchitecture_ImportsPointDown: that one fails when a REAL
// on-disk package has no entry here, this one fails when an entry here names
// a package that is no longer on disk at all — a GHOST placement, like the
// "okf"/"knowledge" lines this test replaces (W2/U03A deleted the built-in
// :11436 knowledge service package, not just its callers, and the placement
// entries were left behind describing a package nobody could open any more).
// "." is exempt: it names the pix-host daemon's own root package, which
// scanPackages reports under "." too (see its placement comment above), so it
// is real, not a ghost — the exemption is structural, not a loophole.
func TestArchitecture_NoGhostPlacements(t *testing.T) {
	pkgs := scanPackages(t)
	var ghosts []string
	for p := range pkgLayer {
		if p == "." {
			continue
		}
		if _, ok := pkgs[p]; !ok {
			ghosts = append(ghosts, p)
		}
	}
	sort.Strings(ghosts)
	if len(ghosts) > 0 {
		t.Fatalf("pkgLayer places %v, but no such production package exists on disk any more.\n"+
			"A deleted package's placement is a ghost entry describing an architecture that no longer "+
			"exists — remove it from pkgLayer (and l0Order, if it was L0).", ghosts)
	}
}

// TestArchitecture_DrainingListIsShrinking guards the exemption itself. A list
// of "packages allowed to break the rules" only stays honest if adding to it is
// a deliberate, visible act.
func TestArchitecture_DrainingListIsShrinking(t *testing.T) {
	if len(drainingPackages) > 0 {
		t.Errorf("drainingPackages has %d entries; it is empty and may only stay that way.\n"+
			"Every package in this module satisfies the layering. A new package must do so on "+
			"the day it is created; an entry here needs an argument, in the commit message, for "+
			"why the extraction cannot land in one step.", len(drainingPackages))
	}
}

// scanPackages returns pkg-path -> its intra-module imports, also
// pkg-path-relative.
func scanPackages(t *testing.T) map[string][]string {
	t.Helper()
	const mod = "pix/host"
	out := map[string][]string{}
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, path)
		files, _ := filepath.Glob(filepath.Join(path, "*.go"))
		if len(files) == 0 {
			return nil
		}
		var imports []string
		fset := token.NewFileSet()
		hasProd := false
		for _, f := range files {
			// Test files are excluded: a test may legitimately reach for a fake in
			// another layer, and policing that would punish good tests.
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			hasProd = true
			parsed, perr := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
			if perr != nil {
				continue
			}
			for _, spec := range parsed.Imports {
				p := strings.Trim(spec.Path.Value, `"`)
				if strings.HasPrefix(p, mod+"/") {
					imports = append(imports, strings.TrimPrefix(p, mod+"/"))
				}
			}
		}
		// A package with ONLY _test.go files is test-only support, not part of
		// the production architecture: it has no layer to place and no imports
		// to police (see cmd/pix/corpus, reclassified in U11k). Skipping it here
		// keeps the unplaced-package check honest — it fires for a genuine new
		// production package, not for a package that stopped shipping one.
		if !hasProd {
			return nil
		}
		out[filepath.ToSlash(rel)] = imports
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
