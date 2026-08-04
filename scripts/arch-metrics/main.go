// Command arch-metrics measures the per-package architecture health signals
// docs/design/architecture.md and the architecture-audit skill's `fanout` role
// already ask a human to gather by eye: production/test LOC, the exported
// surface (and how much of it a DIFFERENT package's production code actually
// calls), os.Exit calls, concurrency constructs, package-level globals, init()
// functions, intra-module import fan-out, and the file-family split this repo
// already uses by convention (`*_cmd.go` argv seams vs core vs test).
//
// It runs in three modes, composable on one invocation:
//
//	go run ./scripts/arch-metrics -root services/host                    # print current metrics
//	go run ./scripts/arch-metrics -root services/host -budgets FILE       # shrink-only CHECK
//	go run ./scripts/arch-metrics -root services/host -write-budgets FILE # seed/ratchet budgets
//
// WHY THESE FIELDS ARE SHRINK-ONLY-GATED AND OTHERS ARE NOT: the
// architecture-audit skill says it outright — "Prefer fewer types, exports,
// dependencies, globals, branches, and lines." Production LOC, the exported
// surface, globals, intra-module edges, and os.Exit calls are all "less is
// better" complexity signals, so -budgets enforces current <= recorded ceiling
// for exactly those five. Test LOC, concurrency constructs, init() counts, and
// the file-family histogram are recorded for review but never gated: more
// tests, or a legitimate goroutine, is not a regression.
//
// Coverage is intentionally NOT computed here — that needs `go test`, which is
// the expensive, non-AST-only part of the full corpus job. Merge it in with
// -coverage, pointing at a `go test -cover ./...` text log; see
// .github/workflows/test.yml's `metrics` job.
//
// This tool uses go/parser only (no go/types, no `go build`), the same choice
// scripts/extract-pkg made and documented: fast and precise enough for
// declarations and call shapes, at the cost of a purely syntactic (not
// type-accurate) notion of "which package does this selector belong to". A
// selector's alias is resolved against that FILE's own import list, which is
// enough to answer "does any production file outside this package call
// pkg.Symbol" without a full compile.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// PkgMetrics is the full set of derived signals for one package.
type PkgMetrics struct {
	ProdLOC               int            `json:"prod_loc"`
	TestLOC               int            `json:"test_loc"`
	Exports               int            `json:"exports"`
	ExportsUsedExternally int            `json:"exports_used_externally"`
	Exits                 int            `json:"exits"`
	Streams               int            `json:"streams"`
	Globals               int            `json:"globals"`
	Init                  int            `json:"init"`
	Edges                 int            `json:"edges"`
	ParserFamilies        map[string]int `json:"parser_families"`
	CoveragePct           *float64       `json:"coverage_pct,omitempty"`
}

// Report is the full snapshot this tool emits with -out/stdout.
type Report struct {
	Schema      int                   `json:"schema"`
	GeneratedAt string                `json:"generated_at"`
	Module      string                `json:"module"`
	Packages    map[string]PkgMetrics `json:"packages"`
}

// Budget is the shrink-only-gated SUBSET of PkgMetrics. See the file doc
// comment for which fields these are and why.
type Budget struct {
	ProdLOC int `json:"prod_loc"`
	Exports int `json:"exports"`
	Globals int `json:"globals"`
	Edges   int `json:"edges"`
	Exits   int `json:"exits"`
}

// Budgets is the committed, tracked ratchet file.
type Budgets struct {
	Schema   int               `json:"schema"`
	Packages map[string]Budget `json:"packages"`
}

func main() {
	root := flag.String("root", ".", "module root to scan (directory containing go.mod)")
	out := flag.String("out", "", "write the current-metrics JSON here (default: stdout)")
	coverage := flag.String("coverage", "", "merge per-package coverage_pct from a `go test -cover ./...` text log")
	budgetsPath := flag.String("budgets", "", "shrink-only CHECK: fail if any package exceeds its recorded ceiling")
	writeBudgets := flag.String("write-budgets", "", "seed/ratchet the budgets file at this path from current metrics (never raises an existing ceiling)")
	flag.Parse()

	report, err := scan(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "arch-metrics:", err)
		os.Exit(2)
	}

	if *coverage != "" {
		cov, err := parseCoverage(*coverage)
		if err != nil {
			fmt.Fprintln(os.Stderr, "arch-metrics: coverage:", err)
			os.Exit(2)
		}
		for pkg, pct := range cov {
			if m, ok := report.Packages[pkg]; ok {
				v := pct
				m.CoveragePct = &v
				report.Packages[pkg] = m
			}
		}
	}

	if err := emit(report, *out); err != nil {
		fmt.Fprintln(os.Stderr, "arch-metrics:", err)
		os.Exit(2)
	}

	exitCode := 0
	if *budgetsPath != "" {
		violations, err := checkBudgets(report, *budgetsPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "arch-metrics:", err)
			os.Exit(2)
		}
		if len(violations) > 0 {
			fmt.Fprintln(os.Stderr, "arch-metrics: shrink-only budget violations:")
			for _, v := range violations {
				fmt.Fprintln(os.Stderr, "  "+v)
			}
			exitCode = 1
		}
	}

	if *writeBudgets != "" {
		if err := writeBudgetsFile(report, *writeBudgets); err != nil {
			fmt.Fprintln(os.Stderr, "arch-metrics: write-budgets:", err)
			os.Exit(2)
		}
	}

	os.Exit(exitCode)
}

func emit(report *Report, out string) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if out == "" {
		_, err = os.Stdout.Write(b)
		return err
	}
	if dir := filepath.Dir(out); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(out, b, 0o644)
}

// --- scanning ----------------------------------------------------------------

type parsedFile struct {
	path    string
	pkg     string // module-relative package path this file belongs to
	isTest  bool
	imports map[string]string // local alias -> full import path (this file only)
	file    *ast.File
	lines   int
}

func scan(root string) (*Report, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	module, err := readModuleName(absRoot)
	if err != nil {
		return nil, err
	}

	var files []parsedFile
	fset := token.NewFileSet()
	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(absRoot, filepath.Dir(path))
		rel = filepath.ToSlash(rel)
		pkgPath := module
		if rel != "." {
			pkgPath = module + "/" + rel
		}
		parsed, perr := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", path, perr)
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		imports := map[string]string{}
		for _, spec := range parsed.Imports {
			p := strings.Trim(spec.Path.Value, `"`)
			alias := filepath.Base(p)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			imports[alias] = p
		}
		files = append(files, parsedFile{
			path:    path,
			pkg:     pkgPath,
			isTest:  strings.HasSuffix(path, "_test.go"),
			imports: imports,
			file:    parsed,
			lines:   countLines(content),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no .go files found under %s — the walk found nothing, which is itself a bug worth failing on", root)
	}

	packages := map[string]*PkgMetrics{}
	pkgOf := func(p string) *PkgMetrics {
		m, ok := packages[p]
		if !ok {
			m = &PkgMetrics{ParserFamilies: map[string]int{}}
			packages[p] = m
		}
		return m
	}

	// exportedIn[pkgPath][identifier] = true for every exported top-level
	// identifier declared in that package's PRODUCTION files.
	exportedIn := map[string]map[string]bool{}
	// externallyUsed[pkgPath][identifier] = true once a PRODUCTION file in a
	// DIFFERENT package is observed calling pkg.identifier.
	externallyUsed := map[string]map[string]bool{}

	for _, f := range files {
		m := pkgOf(f.pkg)
		family := fileFamily(f.path, f.isTest)
		m.ParserFamilies[family]++

		if f.isTest {
			m.TestLOC += f.lines
			continue
		}
		m.ProdLOC += f.lines

		ast.Inspect(f.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.ChanType:
				m.Streams++
			case *ast.GoStmt:
				m.Streams++
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					if x, ok := sel.X.(*ast.Ident); ok {
						if imp, ok := f.imports[x.Name]; ok {
							if imp == "os" && sel.Sel.Name == "Exit" {
								m.Exits++
							}
							if imp != f.pkg && strings.HasPrefix(imp, module) {
								if externallyUsed[imp] == nil {
									externallyUsed[imp] = map[string]bool{}
								}
								externallyUsed[imp][sel.Sel.Name] = true
							}
						}
					}
				}
			case *ast.GenDecl:
				if n.Tok == token.VAR || n.Tok == token.CONST {
					// GenDecl at ast.Inspect can also appear nested inside a
					// function body (a local var/const block); only top-level
					// declarations are globals. Distinguish by checking this
					// GenDecl is one of the file's own Decls.
					if isTopLevel(f.file, n) {
						for _, spec := range n.Specs {
							if vs, ok := spec.(*ast.ValueSpec); ok {
								m.Globals += len(vs.Names)
								for _, name := range vs.Names {
									if name.IsExported() {
										markExported(exportedIn, f.pkg, name.Name)
									}
								}
							}
						}
					}
				}
				if n.Tok == token.TYPE {
					for _, spec := range n.Specs {
						if ts, ok := spec.(*ast.TypeSpec); ok && ts.Name.IsExported() && isTopLevel(f.file, n) {
							markExported(exportedIn, f.pkg, ts.Name.Name)
						}
					}
				}
			case *ast.FuncDecl:
				if n.Recv == nil && n.Name.Name == "init" {
					m.Init++
				}
				if n.Recv == nil && n.Name.IsExported() {
					markExported(exportedIn, f.pkg, n.Name.Name)
				}
			}
			return true
		})
	}

	// Recompute edges per package as the UNION across its own production
	// files' intra-module imports (a single file's count would undercount a
	// multi-file package).
	pkgEdgeSets := map[string]map[string]bool{}
	for _, f := range files {
		if f.isTest {
			continue
		}
		set := pkgEdgeSets[f.pkg]
		if set == nil {
			set = map[string]bool{}
			pkgEdgeSets[f.pkg] = set
		}
		for imp := range collectIntraModuleImports(f, module) {
			set[imp] = true
		}
	}
	for pkg, set := range pkgEdgeSets {
		pkgOf(pkg).Edges = len(set)
	}

	for pkg, idents := range exportedIn {
		m := pkgOf(pkg)
		m.Exports = len(idents)
		used := externallyUsed[pkg]
		count := 0
		for id := range idents {
			if used[id] {
				count++
			}
		}
		m.ExportsUsedExternally = count
	}

	out := map[string]PkgMetrics{}
	for pkg, m := range packages {
		out[pkg] = *m
	}
	return &Report{
		Schema:      1,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Module:      module,
		Packages:    out,
	}, nil
}

func markExported(m map[string]map[string]bool, pkg, name string) {
	if m[pkg] == nil {
		m[pkg] = map[string]bool{}
	}
	m[pkg][name] = true
}

func isTopLevel(f *ast.File, decl ast.Decl) bool {
	for _, d := range f.Decls {
		if d == ast.Decl(decl) {
			return true
		}
	}
	return false
}

func collectIntraModuleImports(f parsedFile, module string) map[string]bool {
	out := map[string]bool{}
	for _, imp := range f.imports {
		if imp == f.pkg {
			continue
		}
		if imp == module || strings.HasPrefix(imp, module+"/") {
			out[imp] = true
		}
	}
	return out
}

func fileFamily(path string, isTest bool) string {
	base := filepath.Base(path)
	switch {
	case isTest:
		return "test"
	case strings.HasSuffix(base, "_cmd.go"):
		return "cmd"
	default:
		return "core"
	}
}

func countLines(content []byte) int {
	if len(content) == 0 {
		return 0
	}
	n := 0
	for _, b := range content {
		if b == '\n' {
			n++
		}
	}
	if content[len(content)-1] != '\n' {
		n++
	}
	return n
}

func readModuleName(root string) (string, error) {
	content, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module")), nil
		}
	}
	return "", fmt.Errorf("no module line found in %s/go.mod", root)
}

// --- coverage ----------------------------------------------------------------

var coverageLineRE = regexp.MustCompile(`^ok\s+(\S+)\s+\S+\s+coverage:\s+([\d.]+)% of statements`)

// parseCoverage reads the TEXT output of `go test -cover ./...` and returns
// pkgPath -> coverage percentage. Packages with no test files ("?  pkg  [no
// test files]") are simply absent, not zero — absence and zero-with-tests are
// different findings and this tool does not conflate them.
func parseCoverage(path string) (map[string]float64, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, line := range strings.Split(string(content), "\n") {
		m := coverageLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		pct, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		out[m[1]] = pct
	}
	return out, nil
}

// --- budgets -------------------------------------------------------------------

func toBudget(m PkgMetrics) Budget {
	return Budget{ProdLOC: m.ProdLOC, Exports: m.Exports, Globals: m.Globals, Edges: m.Edges, Exits: m.Exits}
}

func loadBudgets(path string) (*Budgets, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Budgets{Schema: 1, Packages: map[string]Budget{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var b Budgets
	if err := json.Unmarshal(content, &b); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if b.Packages == nil {
		b.Packages = map[string]Budget{}
	}
	return &b, nil
}

// checkBudgets is the shrink-only gate: every gated field of every RECORDED
// package must be <= its budget. A package absent from the budgets file is not
// a violation (it is new, or the budgets file has not been seeded yet) — that
// is what keeps seeding the file a non-breaking, separate step from adding the
// check, per the "without making current baseline fail" requirement.
func checkBudgets(report *Report, path string) ([]string, error) {
	budgets, err := loadBudgets(path)
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for p := range report.Packages {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	var violations []string
	for _, pkg := range pkgs {
		budget, ok := budgets.Packages[pkg]
		if !ok {
			continue
		}
		cur := toBudget(report.Packages[pkg])
		check := func(field string, curV, budgetV int) {
			if curV > budgetV {
				violations = append(violations, fmt.Sprintf("%s: %s grew %d -> %d (budget %d)", pkg, field, budgetV, curV, budgetV))
			}
		}
		check("prod_loc", cur.ProdLOC, budget.ProdLOC)
		check("exports", cur.Exports, budget.Exports)
		check("globals", cur.Globals, budget.Globals)
		check("edges", cur.Edges, budget.Edges)
		check("exits", cur.Exits, budget.Exits)
	}
	return violations, nil
}

// writeBudgetsFile seeds new packages and RATCHETS existing ones down when a
// package genuinely shrank. It refuses to raise an existing ceiling — that
// would defeat the entire point of a shrink-only budget and turn it into a
// snapshot that happens to be called a budget.
func writeBudgetsFile(report *Report, path string) error {
	budgets, err := loadBudgets(path)
	if err != nil {
		return err
	}
	var pkgs []string
	for p := range report.Packages {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	next := map[string]Budget{}
	for _, pkg := range pkgs {
		cur := toBudget(report.Packages[pkg])
		if existing, ok := budgets.Packages[pkg]; ok {
			next[pkg] = Budget{
				ProdLOC: minInt(existing.ProdLOC, cur.ProdLOC),
				Exports: minInt(existing.Exports, cur.Exports),
				Globals: minInt(existing.Globals, cur.Globals),
				Edges:   minInt(existing.Edges, cur.Edges),
				Exits:   minInt(existing.Exits, cur.Exits),
			}
			// A caller asking to write a HIGHER value than the recorded ceiling
			// (i.e. the current tree genuinely grew) must not silently ratchet up.
			if cur.ProdLOC > existing.ProdLOC || cur.Exports > existing.Exports ||
				cur.Globals > existing.Globals || cur.Edges > existing.Edges || cur.Exits > existing.Exits {
				return fmt.Errorf("%s grew past its recorded budget (%+v -> %+v); "+
					"shrink the code, or edit budgets.json by hand with a reason in the commit message — "+
					"this tool will not raise a ceiling silently", pkg, existing, cur)
			}
		} else {
			next[pkg] = cur
		}
	}

	out := Budgets{Schema: 1, Packages: next}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
