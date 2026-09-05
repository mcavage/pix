package env

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"pix/host/envinfo"
)

// fullBoM is a bill of materials with EVERY fingerprinted section populated,
// so a mutation test can move one section at a time and know the others are
// non-empty (an empty section can hide an ordering or keying bug).
func fullBoM() BillOfMaterials {
	def := "fallback"
	return BillOfMaterials{
		HostCommands:      []HostCommand{{Name: "gog", Argv: []string{"gog", "mcp"}}},
		HostServices:      []HostServiceItem{{Name: "snow-proxy", Command: "snow-proxy", Args: []string{"--port", "11442"}, Port: 11442, Probe: "http://127.0.0.1:11442/health"}},
		SetupHooks:        []SetupHookFact{{ID: "slack", Kind: "auth", Command: "./setup/slack", CheckArgs: []string{"check"}, ApplyArgs: []string{"apply"}, Required: true, SHA: "aaa"}},
		CredentialTargets: []CredentialTarget{{Source: "BAMBOOHR_API_KEY", Destination: "bamboohr"}},
		EffectiveMounts:   EffectiveMounts{{Path: "/src", ReadOnly: false}},
		Secrets:           []SecretFact{{Name: "github", Ref: "op://v/i/f"}},
		Registries:        []RegistryFact{{Host: "registry.example.com", Ref: "op://v/i/f", NoVerify: true}},
		Bindings:          []BindingFact{{Service: "sbx-login", Domains: []string{"gw.docker.com"}}},
		MCPServers:        []MCPServerFact{{Name: "bamboohr", Command: "docker", Args: []string{"run", "bamboohr-mcp:0.0.1"}}},
		Ports:             []PortFact{{Sandbox: 8080, Host: 8080}},
		Kits:              []KitFact{{Raw: "kit/spec.yaml", Resolved: "/env/kit/spec.yaml", Local: true, SHA: "bbb"}, {Raw: "pi-kit/spec.yaml", Resolved: "/pix/pi-kit/spec.yaml", Local: true, SHA: "ccc"}},
		HostMCP:           []HostMCPFact{{Name: "google-workspace", EnvKeys: []string{"GOG_KEYRING_PASSWORD"}, PlainKeys: []string{"GOG_ACCOUNT"}, ProbeArgs: []string{"gog", "--readonly"}}},
		Inference:         []InferenceFact{{Name: "docker-anthropic", Driver: "openai-compatible", BaseURL: "https://gw.docker.com", Auth: "sbx-session", KeyEnv: "DOCKER_TOKEN", CredentialService: "sbx-login"}},
		Interpolations:    []envinfo.Interpolation{{Var: "HOME", Default: &def, KeyPath: "workspaces[0].path"}},
	}
}

// TestFingerprintChangeAlwaysDiffs is the load-bearing property: a change
// screen must never be able to say "nothing changed" while the gate is
// refusing entry on a fingerprint mismatch. Every section is mutated in
// turn, including a pure REORDER of the two order-significant ones.
func TestFingerprintChangeAlwaysDiffs(t *testing.T) {
	base := fullBoM()
	baseFP, err := Fingerprint(base)
	if err != nil {
		t.Fatalf("fingerprint base: %v", err)
	}
	baseReceipt, err := Receipt(base)
	if err != nil {
		t.Fatalf("receipt base: %v", err)
	}

	mutations := map[string]func(b *BillOfMaterials){
		"host command argv":     func(b *BillOfMaterials) { b.HostCommands[0].Argv = []string{"gog", "mcp", "--readonly"} },
		"host command added":    func(b *BillOfMaterials) { b.HostCommands = append(b.HostCommands, HostCommand{Name: "evil"}) },
		"host command removed":  func(b *BillOfMaterials) { b.HostCommands = nil },
		"host service port":     func(b *BillOfMaterials) { b.HostServices[0].Port = 1 },
		"setup hook sha":        func(b *BillOfMaterials) { b.SetupHooks[0].SHA = "zzz" },
		"setup hook apply argv": func(b *BillOfMaterials) { b.SetupHooks[0].ApplyArgs = []string{"apply", "--force"} },
		"credential dest":       func(b *BillOfMaterials) { b.CredentialTargets[0].Destination = "elsewhere" },
		"mount writability":     func(b *BillOfMaterials) { b.EffectiveMounts[0].ReadOnly = true },
		"secret ref":            func(b *BillOfMaterials) { b.Secrets[0].Ref = "op://other/i/f" },
		"registry no-verify":    func(b *BillOfMaterials) { b.Registries[0].NoVerify = false },
		"binding domain":        func(b *BillOfMaterials) { b.Bindings[0].Domains = []string{"evil.example.com"} },
		"mcp server args":       func(b *BillOfMaterials) { b.MCPServers[0].Args = []string{"run", "--privileged", "x"} },
		"port mapping":          func(b *BillOfMaterials) { b.Ports[0].Host = 9999 },
		"kit sha":               func(b *BillOfMaterials) { b.Kits[0].SHA = "zzz" },
		"kit reorder":           func(b *BillOfMaterials) { b.Kits[0], b.Kits[1] = b.Kits[1], b.Kits[0] },
		"host mcp env keys":     func(b *BillOfMaterials) { b.HostMCP[0].EnvKeys = []string{"OTHER"} },
		"host mcp plain keys":   func(b *BillOfMaterials) { b.HostMCP[0].PlainKeys = nil },
		"inference credential":  func(b *BillOfMaterials) { b.Inference[0].CredentialService = "other" },
		"interpolation target":  func(b *BillOfMaterials) { b.Interpolations[0].KeyPath = "secrets[0].ref" },
	}

	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			got := fullBoM()
			mutate(&got)
			fp, err := Fingerprint(got)
			if err != nil {
				t.Fatalf("fingerprint: %v", err)
			}
			if fp == baseFP {
				t.Fatalf("mutation %q did not change the fingerprint; this test's premise is broken", name)
			}
			rec, err := Receipt(got)
			if err != nil {
				t.Fatalf("receipt: %v", err)
			}
			if diff := DiffReceipts(baseReceipt, rec); len(diff) == 0 {
				t.Fatalf("fingerprint changed but the receipt diff is empty for %q: the change screen would contradict the gate", name)
			}
		})
	}
}

// TestReceiptStableForUnchangedBoM: no diff, no noise. A re-computed receipt
// of the same bill of materials must be identical, or every launch would
// print a change screen.
func TestReceiptStableForUnchangedBoM(t *testing.T) {
	a, err := Receipt(fullBoM())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Receipt(fullBoM())
	if err != nil {
		t.Fatal(err)
	}
	if receiptDigestOfEntries(a) != receiptDigestOfEntries(b) {
		t.Fatal("receipt is not stable across two computations of the same bill of materials")
	}
	if diff := DiffReceipts(a, b); len(diff) != 0 {
		t.Fatalf("unchanged bill of materials produced %d change(s): %+v", len(diff), diff)
	}
	if got, want := UnchangedCount(b, nil), len(b); got != want {
		t.Fatalf("UnchangedCount = %d, want %d", got, want)
	}
}

// TestDiffNamesTheRightThing pins the reading a human gets: one added, one
// removed, one changed, and an accurate count of what was left alone.
func TestDiffNamesTheRightThing(t *testing.T) {
	prev, err := Receipt(fullBoM())
	if err != nil {
		t.Fatal(err)
	}
	next := fullBoM()
	next.SetupHooks[0].SHA = "edited"                                             // changed
	next.HostCommands = append(next.HostCommands, HostCommand{Name: "new-thing"}) // added
	next.Secrets = nil                                                            // removed
	cur, err := Receipt(next)
	if err != nil {
		t.Fatal(err)
	}
	diff := DiffReceipts(prev, cur)
	got := map[string]ChangeKind{}
	for _, c := range diff {
		got[c.Section+" "+c.Key] = c.Kind
	}
	want := map[string]ChangeKind{
		"host command new-thing": ChangeAdded,
		"setup hook slack":       ChangeChanged,
		"secret github":          ChangeRemoved,
	}
	if len(got) != len(want) {
		t.Fatalf("diff = %+v, want exactly %d changes", diff, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("change for %q = %q, want %q (full diff %+v)", k, got[k], v, diff)
		}
	}
	// Everything else is untouched: the two facts still present and not
	// named (the added one is named, the removed one is not in cur).
	if n, total := UnchangedCount(cur, diff), len(cur); n != total-2 {
		t.Fatalf("UnchangedCount = %d of %d, want %d", n, total, total-2)
	}
}

// TestReceiptCoversEveryFingerprintedSection is the anti-rot guard: fpDoc is
// what re-gates an environment, so a section added there without a matching
// Receipt line would silently become invisible on the change screen — the
// diff would report "nothing changed" for a real change, the exact failure
// TestFingerprintChangeAlwaysDiffs exists to prevent but could not see for a
// section nobody thought to add to its mutation table.
func TestReceiptCoversEveryFingerprintedSection(t *testing.T) {
	fset := token.NewFileSet()
	bom, err := parser.ParseFile(fset, "bom.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	ast.Inspect(bom, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "fpDoc" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, f := range st.Fields.List {
			for _, nm := range f.Names {
				fields = append(fields, nm.Name)
			}
		}
		return false
	})
	if len(fields) == 0 {
		t.Fatal("bom.go declares no fpDoc struct fields; this guard cannot see the fingerprint surface")
	}
	src, err := parser.ParseFile(token.NewFileSet(), "receipt.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var body strings.Builder
	ast.Inspect(src, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Receipt" {
			return true
		}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			if sel, ok := m.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "doc" {
					body.WriteString(sel.Sel.Name)
					body.WriteString("\n")
				}
			}
			return true
		})
		return false
	})
	for _, f := range fields {
		if !strings.Contains(body.String(), f+"\n") {
			t.Fatalf("fpDoc field %q is fingerprinted but Receipt never reads doc.%s: a change to it would re-gate the environment and then show up as no change at all", f, f)
		}
	}
}
