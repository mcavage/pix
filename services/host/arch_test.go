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
//	L0  foundation     sys, config, rpc, cli, hostenv
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

	// L1 — capability. One domain each, siblings invisible to each other.
	//
	// okf and knowledge (the built-in OKF knowledge service, :11436), and
	// `unitreport`/`unitreport/unitreporttest`, `service`, `supervise`, `plugin`,
	// `memory` (the pre-v2 host memory CLI + custom JSON-RPC), `rpc`,
	// `packinfo`, `uat`/`uatmatrix`/`uatenvmatrix`, `workflow/pack` and
	// `workflow/uat` are deliberately ABSENT from this map: the Pix v2 deletion
	// sweep (docs/design/pix-v2-architecture.md §14, AC-16) removed
	// `pix-host serve`'s Suture supervision tree, the pack system, the
	// self-development UAT candidate harness, and the custom memory JSON-RPC
	// daemon outright, not merely their callers. A placement entry for a
	// package that no longer exists on disk is a GHOST — see
	// TestArchitecture_NoGhostPlacements below, which fails the day one lingers
	// instead of relying on someone noticing.
	"inference": layerCapability,
	"secret":    layerCapability,
	"mcp":       layerCapability,
	// sandbox is U04b's focused L1 sandbox domain: naming, the tolerant sbx-
	// listing parser, create-vs-exec argv planning, fingerprint comparison and
	// non-force removal planning. Pure and dependency-free (see sandbox/doc.go),
	// same tier as its capability siblings, invisible to them.
	"sandbox": layerCapability,
	// session is the v2 session tree (docs/design/pix-v2-architecture.md
	// §7): tree/node records plus the flock reference holders that decide
	// whether a sandbox is still claimed. Like sandbox and lease it is pure
	// mechanism over the filesystem — stdlib only, no sibling imports — so
	// launch can build the holder census on it without dragging a workflow
	// dependency down into a capability.
	"session": layerCapability,
	// hosttrust is E1.4's launcher-owned host-exec trust mechanism, extracted
	// from the (now-deleted) pack system: canonical identity, the fingerprint
	// engine, the one Record/AcceptanceStore shape keyed by an opaque
	// Subject{Kind,Root}, the flock-serialized fresh-load->mutate->save shape,
	// symlink-refused atomic document I/O, and content hashing. Pure mechanism
	// only — it has no knowledge of environments or anything else that HAS a
	// host-exec surface — so, like every other L1 capability, it imports no
	// sibling.
	"hosttrust": layerCapability,
	// workflow/task is Story06's L1 task-checkout capability: naming, metadata,
	// the clone/worktree mechanism, and the git-hygiene removal guard, with NO
	// import of workflow/launch, any sandbox runner, or lease (see its doc
	// comment). It lives under workflow/ for discoverability next to the CLI
	// surface it backs, but it is a leaf capability, not a workflow: it composes
	// nothing below it and orchestrates no other capability.
	"workflow/task": layerCapability,
	// envinfo is Story 1's L1 native-environment capability (docs/design/
	// environments.md §5.1): strict `.sbxenv.yaml` v1 parsing, upstream's
	// documented multi-file merge semantics, local relative kit-path
	// resolution against the SOURCE FILE's own directory, and the
	// PRE-COMPOSITION semantic tree (stable identity key paths, host-exec
	// facets, surfaced-never-resolved `${VAR}` interpolation references).
	// It imports no sibling capability — see envinfo/doc.go — and it takes
	// no dependency on `sandbox`: a caller supplies any rendered `pix-*`
	// name itself. It is the ONE package allowed to decode a struct tagged
	// `yaml:"schemaVersion"` for this schema; see
	// TestOnlyEnvinfoDecodesNativeEnvYAML below.
	"envinfo": layerCapability,
	// recreatelog is E1.6's local-only bounded diagnostic log of environment
	// recreate-boundary drift (docs/design/environments.md section 10.2):
	// timestamp, environment name, and canonical changed key paths only, never
	// facet values, credential names, argv, or a path outside the environment
	// root (see its doc.go). It has NO L1 siblings in the load-bearing sense —
	// it imports nothing from this module at all (recreatelog/guard_test.go's
	// F10 pins that from inside the package too) and nothing else in L1 imports
	// it; wiring it into a reader (doctor or otherwise) is explicitly deferred
	// past this unit.
	"recreatelog": layerCapability,
	// container is this unit's Docker adapter + reconciliation for the one
	// named pix-memory container (docs/design/pix-v2-architecture.md §9.1):
	// docker inspect/create/start/stop/rm argv, the config fingerprint that
	// decides adopt vs start vs replace, and the injectable post-reconcile
	// readiness Prober. Pure docker-CLI + HTTP mechanism, no sibling import,
	// same tier as sandbox and session.
	"container": layerCapability,

	// pixhome is Pix v2 U1's PIX_HOME resolution + home-directory layout and
	// idempotent initialization (docs/design/pix-v2-architecture.md §5): where
	// is the one root, what lives under it, and how `git init -b main` plus
	// the fixed directory/README/.gitignore set get created without ever
	// overwriting something already there. It is L0 like config and workspace
	// — pure path/location resolution, no domain knowledge — but it imports
	// nothing from this module at all, unlike config's still-XDG-aware
	// StateDir/DataDir. It is deliberately independent of config: collapsing
	// the REST of the launcher's paths onto PIX_HOME is a later cutover step
	// (architecture §13 step 4), not U1.
	"pixhome": layerFoundation,
	// unitreport, unitreport/unitreporttest, plugin, service, memory (the
	// pre-v2 host memory CLI), and the custom memory JSON-RPC package rpc are
	// deleted — see the L1 comment above.
	// release is Pix v2 U1's release-manifest schema (docs/design/
	// pix-v2-architecture.md §3, §12): the one document binding a Pix version
	// to the pix-agent digest, pix-memory digest, runtime archive digest, and
	// kit revision, its parser/validation, and the on-disk install-state
	// location. It takes a home path as a plain string parameter rather than
	// importing pixhome, so it has no intra-module imports either.
	"release": layerFoundation,

	// L2 — the shared model of "is this working".
	//
	// supervise (the Suture process supervisor that ran `plugin` capability
	// units) is deleted with `pix-host serve` — see the L1 comment above.
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
	// workflow/reset is `pix reset`, BACK after U11r cut it: the manual,
	// evidence-first walk that replaced it (`pix doctor` -> `pix config path` ->
	// move things aside by hand -> `pix setup`) is the right RECOVERY story but
	// was never a clean-slate story, and "start over" is a thing users ask for
	// on its own. What U11r was actually right about is preserved in the shape,
	// not the absence: nothing durable is hard-deleted, and the sandbox half is
	// an INJECTED sweep (reset.Sweep) wired to `pix rm --all` by cmd/pix, so the
	// L3-to-L3 import this file forbids never happens AND reset cannot become a
	// second bulk force-removal seam beside the one explicitly-named one.
	//
	// `pix state reset` did NOT come back with it: `state` was a grouping noun
	// for backup/restore/reset, and two of those three are still gone.
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
	"workflow/doctor": layerWorkflow,
	// workflow/env is E1.7's new L3 workflow (native-environment registry,
	// exact-name resolution, location/symlink refusals — docs/design/
	// environments.md §5.3/§8.1). It is filed under workflow/ like
	// workflow/task, but unlike that leaf capability it IS a workflow: it
	// composes config (L0) with the hosttrust and envinfo L1 capabilities
	// and orchestrates nothing beneath it directly. It imports no other
	// workflow/* package (the sibling-workflow rule below) and carries no
	// `pix env` verb yet — that is E1.9-E1.13's cmd/pix wiring.
	"workflow/env":    layerWorkflow,
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
	"workflow/reset": layerWorkflow,

	// L4 — the command layer.
	"cmd/pix": layerCommand,
	// cmd/pix/corpus (the W0 U00b golden CLI corpus + retirement-manifest
	// harness) is deliberately ABSENT from this map: U11k reclassified it as
	// test-only support (every file under it is now a _test.go file, so it
	// has zero production LOC and no runtime caller). scanPackages below
	// skips packages with no production .go files for exactly this reason —
	// a package that is entirely tests has no layer to place, the same way it
	// has no budget to track in scripts/arch-metrics/budgets.json.
	//
	// "." (the pix-host daemon binary's own root package) is gone the same
	// way: the Pix v2 cutover deleted pix-host outright (AC-16), so services/
	// host's root directory carries no production .go file any more — only
	// this file and arch_effective_test.go, which scanPackages already
	// excludes. A ghost "." entry here would fail
	// TestArchitecture_NoGhostPlacements the same as any other deleted package.
}

// l0Order breaks ties inside L0, which is the one layer with internal
// structure: pixhome, sys, and config are the true bottom, and cli/hostenv
// are allowed to build on them. Without this, "equal layers may not import
// each other" would forbid cli from importing sys, which is nonsense.
// Ranks, lowest first: pixhome is PIX_HOME resolution with no dependencies at
// all (stdlib only, docs/design ledger item "old config XDG fallback" — QA
// F5); config is a pure file format that now delegates its own path
// resolution to pixhome (config.Path/StateDir/DataDir all route through
// PIX_HOME, never a second XDG root) so it sits one rank above; sys sits
// above that because sys.Real.StateDir delegates to config, which is
// correct — the OS seam should not re-derive the launcher's data layout.
var l0Order = map[string]int{
	"pixhome": 0,
	"config":  1,
	"sys": 2, "launcher": 2,
	"workspace":   3,
	"sys/systest": 3, "hostenv": 4,
	"cli": 5,
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

// TestArchitecture_UatenvmatrixNeverImportsEnvinfo (the uatenvmatrix host-
// backed native-environment probe never importing envinfo's own renderer,
// ADR-ENV-003) was deleted along with the uatenvmatrix package itself: the
// self-development UAT candidate-testing harness it backed went with
// `pix-host` in the Pix v2 cutover (AC-16). There is no longer a second
// prover of the upstream `.sbxenv.yaml` contract to keep independent of
// envinfo's own renderer.

// TestOnlyEnvinfoDecodesNativeEnvYAML (F3) is the import-graph guard the
// implementation plan asks for: envinfo/document.go is the ONE place in
// this module allowed to declare a struct field tagged
// `yaml:"schemaVersion"` — the exact tag a native `.sbxenv.yaml` v1
// decode target must carry (envinfo/document.go's own Document.SchemaVersion
// field). A second package growing that tag would be a second, silently
// diverging native-env parser/loader, which is precisely the "one package
// owns native sbx env grammar" fitness function docs/design/environments.md
// section 14 lists. This scans Go SOURCE TEXT rather than only import
// edges, because uatenvmatrix's own fixtures.go legitimately contains the
// literal bytes `schemaVersion:` inside hand-authored YAML string
// constants (never a struct tag) — a decode-target guard must tell
// "declares a field for this" apart from "embeds these bytes as data",
// which an import-graph check alone cannot.
func TestOnlyEnvinfoDecodesNativeEnvYAML(t *testing.T) {
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	const tag = `yaml:"schemaVersion"`
	var offenders []string
	scanned := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == "envinfo/document.go" {
			return nil
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		if strings.Contains(string(content), tag) {
			offenders = append(offenders, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned < 50 {
		t.Fatalf("walk scanned only %d production .go files; that is implausibly few and means this guard is scanning nothing", scanned)
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("only services/host/envinfo/document.go may declare a %s struct field; also found in: %v\n"+
			"envinfo is the one package that decodes native `.sbxenv.yaml` — see envinfo/doc.go", tag, offenders)
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
