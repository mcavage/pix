package main

// arch_test.go enforces docs/design/architecture.md. It is the whole reason
// that document is a contract rather than aspirational prose: a layering rule
// nothing checks is a layering rule that lasted until the next deadline.
//
// The rule: imports point DOWN, never up, and never sideways within L1.
//
//	L4  cmd/pix        argv -> a command; owns os.Exit
//	L3  workflow/*     orchestrate L1+L2
//	L2  readiness      L1 probes -> a Snapshot
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
	"sys":                 layerFoundation,
	"sys/systest":         layerFoundation,
	"hostenv/hostenvtest": layerFoundation,
	"config":              layerFoundation,
	"routing":             layerFoundation,
	"rpc":                 layerFoundation,
	"cli":                 layerFoundation,
	"hostenv":             layerFoundation,
	"launcher":            layerFoundation,
	// workspace is L0, not a capability: it answers "where am I working and what
	// state is stored there", which is location resolution in the same family as
	// config. It imports only config and sys. Filing it as a capability was a
	// mis-read that made two legitimate users (memory, knowledge) look like
	// sibling violations.
	"workspace": layerFoundation,

	// L1 — capability. One domain each, siblings invisible to each other.
	"inference":   layerCapability,
	"monitor":     layerCapability,
	"monitor/tui": layerCapability,
	"okf":         layerCapability,
	"plugin":      layerCapability,
	"slackoauth":  layerCapability,
	"service":     layerCapability,
	"memory":      layerCapability,
	"knowledge":   layerCapability,
	"secret":      layerCapability,
	"mcp":         layerCapability,

	// L2 — the shared model of "is this working".
	"readiness": layerReadiness,

	// L3 — workflow. A user-facing verb's logic. Allowed to compose L1+L2;
	// may not contain a capability.
	"workflow/backup":  layerWorkflow,
	"workflow/man":     layerWorkflow,
	"workflow/upgrade": layerWorkflow,
	// slack is a WORKFLOW, not a capability, and the layering test is what
	// settled it: as an L1 it violated three rules at once (mcp, secret,
	// readiness). `pix slack setup` sequences an OAuth grant, a credential
	// write, an MCP registration and a readiness report — that is cross-domain
	// sequencing, which is the definition of L3. The pure capability underneath
	// it is slackoauth, which was already L1 and already correct.
	"workflow/slack": layerWorkflow,
	// pack is a workflow for the same reason slack is: `pix pack use` resolves a
	// pack, then registers its MCP servers, wires its knowledge refs, seeds its
	// credentials and restarts services. It consumes five capabilities and
	// nothing below L4 consumes it. The capability-shaped parts inside it
	// (manifest parsing, the trust store, the host BoM) are candidates to split
	// out later; being a workflow is already correct today.
	"workflow/pack": layerWorkflow,

	// L4 — the command layer.
	"cmd/pix": layerCommand,

	// Not part of the launcher's layering: the host daemon binary and its
	// examples, which are separate programs.
	".":                       -1,
	"examples/broker-example": -1,
	"examples/mcp-example":    -1,
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
	"config": 0, "routing": 0,
	"sys": 1, "rpc": 1, "launcher": 1,
	"workspace":   2,
	"sys/systest": 2, "hostenv": 3,
	"hostenv/hostenvtest": 4,
	"cli":                 4,
}

// drainingPackages may still violate the rules while they are being emptied.
// This is the ONLY exemption, and it is a closed list on purpose: the moment it
// becomes a place to add a package, the test stops meaning anything.
var drainingPackages = map[string]bool{
	"cmd/pix": true, // 37k lines still being drained; see architecture.md
}

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

// TestArchitecture_DrainingListIsShrinking guards the exemption itself. A list
// of "packages allowed to break the rules" only stays honest if adding to it is
// a deliberate, visible act.
func TestArchitecture_DrainingListIsShrinking(t *testing.T) {
	if len(drainingPackages) > 1 {
		t.Errorf("drainingPackages has %d entries; it may only shrink.\n"+
			"cmd/pix is the one package still being emptied. Anything else must satisfy the "+
			"layering on the day it is created.", len(drainingPackages))
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
		for _, f := range files {
			// Test files are excluded: a test may legitimately reach for a fake in
			// another layer, and policing that would punish good tests.
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
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
		out[filepath.ToSlash(rel)] = imports
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
