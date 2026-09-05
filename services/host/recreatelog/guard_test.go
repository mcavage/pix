//go:build unix

package recreatelog

// --- F10: recreatelog is local-only — no network, no L1 siblings --------------
//
// recreatelog is a local-only bounded diagnostic log: it never dials out and
// it never reaches sideways into another capability. This guard scans the
// package's own PRODUCTION source (never _test.go, where a fixture is allowed
// to reach for stdlib freely) for two forbidden import shapes:
//
//   - anything network-shaped (net, net/*, crypto/tls): a diagnostic log that
//     records recreate-boundary drift has no reason to ever open a socket.
//   - anything from this module (pix/host/...): recreatelog is placed L1 with
//     NO L1 siblings in arch_test.go's pkgLayer map, and it does not lean on
//     that placement alone — it holds zero internal imports so the sibling
//     ban can never be a live risk here, only a documented one.
//
// TestArchitecture_ImportsPointDown (../arch_test.go) already enforces the
// down-only/no-sibling rule module-wide from the compiled import graph; this
// test is the package-local, self-contained restatement that fails the day
// someone edits recreatelog.go without ever touching arch_test.go.

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func TestF10_NoNetworkOrInternalSiblingImports(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		checked++
		parsed, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, spec := range parsed.Imports {
			p := strings.Trim(spec.Path.Value, `"`)
			if p == "net" || strings.HasPrefix(p, "net/") || p == "crypto/tls" {
				t.Errorf("F10: %s imports network package %q; recreatelog is local-only and must never dial out", f, p)
			}
			if p == "pix/host" || strings.HasPrefix(p, "pix/host/") {
				t.Errorf("F10: %s imports internal package %q; recreatelog is an L1 leaf with no siblings and no internal imports at all", f, p)
			}
		}
	}
	if checked == 0 {
		t.Fatal("F10: found no production .go files to scan — did the package move?")
	}
}
