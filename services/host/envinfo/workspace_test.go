package envinfo

// workspace_test.go — the authored half of sbx 0.39's workspace grammar
// (`workspace:` string-or-object, `additionalWorkspaces:` objects), which
// this package did not model at all until a real host run refused the
// invented `workspaces:` key it modeled instead. See
// upstream_schema_test.go for the rendered half and for the transcribed
// upstream schema both halves are checked against.

import (
	"errors"
	"strings"
	"testing"
)

func parseWorkspaceDoc(t *testing.T, body string) *Document {
	t.Helper()
	doc, err := ParseBytes([]byte(body), "/env/.sbxenv.yaml", "/env")
	if err != nil {
		t.Fatalf("ParseBytes(%q): %v", body, err)
	}
	return doc
}

func TestParse_WorkspaceScalarForm(t *testing.T) {
	doc := parseWorkspaceDoc(t, "schemaVersion: \"1\"\nworkspace: ./web-app\n")
	ws := doc.Workspace
	if !ws.Present || ws.Object {
		t.Fatalf("scalar workspace: Present=%v Object=%v, want true/false", ws.Present, ws.Object)
	}
	if ws.Raw != "./web-app" {
		t.Errorf("Raw = %q, want the authored text ./web-app", ws.Raw)
	}
	if ws.Resolved != "/env/web-app" {
		t.Errorf("Resolved = %q, want it resolved against the environment file's own directory", ws.Resolved)
	}
	if ws.Clone {
		t.Errorf("clone defaulted to true")
	}
}

func TestParse_WorkspaceObjectFormAndAdditional(t *testing.T) {
	doc := parseWorkspaceDoc(t, `schemaVersion: "1"
workspace:
  path: /abs/web-app
  clone: true
additionalWorkspaces:
  - path: ./shared
  - path: /abs/docs
    readOnly: true
`)
	if !doc.Workspace.Object || !doc.Workspace.Clone || doc.Workspace.Resolved != "/abs/web-app" {
		t.Fatalf("object workspace = %+v", doc.Workspace)
	}
	if len(doc.AdditionalWorkspaces) != 2 {
		t.Fatalf("additionalWorkspaces = %+v, want 2 entries", doc.AdditionalWorkspaces)
	}
	if got := doc.AdditionalWorkspaces[0].Resolved; got != "/env/shared" {
		t.Errorf("additionalWorkspaces[0].Resolved = %q, want /env/shared", got)
	}
	if doc.AdditionalWorkspaces[0].ReadOnly {
		t.Errorf("additionalWorkspaces[0] defaulted to read-only")
	}
	if !doc.AdditionalWorkspaces[1].ReadOnly || doc.AdditionalWorkspaces[1].Resolved != "/abs/docs" {
		t.Errorf("additionalWorkspaces[1] = %+v", doc.AdditionalWorkspaces[1])
	}
}

// TestParse_WorkspaceRejectsUnknownNestedFields is the check the union
// type could have quietly lost: yaml.v3's KnownFields(true) does not reach
// through Workspace's custom unmarshaler, so the refusal has to be
// hand-written there. `readOnly` under the PRIMARY workspace is the case
// that matters — upstream has no such field, and accepting it would let an
// author believe a tree is mounted read-only when it is not.
func TestParse_WorkspaceRejectsUnknownNestedFields(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"readOnly on the primary workspace", "schemaVersion: \"1\"\nworkspace:\n  path: ./a\n  readOnly: true\n", "field readOnly not found"},
		{"typo on the primary workspace", "schemaVersion: \"1\"\nworkspace:\n  paths: ./a\n", "field paths not found"},
		{"clone on an additional workspace", "schemaVersion: \"1\"\nadditionalWorkspaces:\n  - path: ./a\n    clone: true\n", "field clone not found"},
		{"typo on an additional workspace", "schemaVersion: \"1\"\nadditionalWorkspaces:\n  - Path: ./a\n", "field Path not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseBytes([]byte(tc.body), "/env/.sbxenv.yaml", "/env")
			if err == nil {
				t.Fatalf("accepted an unknown nested workspace field")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestParse_InterpolatedWorkspacePathIsNotResolved pins the deliberate
// non-resolution: joining the source directory onto `${HOME}/src` would
// fabricate a path nobody authored, and this package resolves no host
// variable of its own.
func TestParse_InterpolatedWorkspacePathIsNotResolved(t *testing.T) {
	doc := parseWorkspaceDoc(t, "schemaVersion: \"1\"\nworkspace: ${PIX_SRC}/app\n")
	if doc.Workspace.Resolved != "${PIX_SRC}/app" {
		t.Errorf("Resolved = %q, want the authored expression carried through verbatim", doc.Workspace.Resolved)
	}
}

func TestMerge_WorkspaceReplacesAndAdditionalConcatenate(t *testing.T) {
	base := parseWorkspaceDoc(t, "schemaVersion: \"1\"\nworkspace: ./base\nadditionalWorkspaces:\n  - path: ./one\n")
	base.Source = "base.sbxenv.yaml"
	over := parseWorkspaceDoc(t, "schemaVersion: \"1\"\nworkspace:\n  path: ./over\n  clone: true\nadditionalWorkspaces:\n  - path: ./two\n    readOnly: true\n")
	over.Source = "local.sbxenv.yaml"

	m, err := Merge(base, over)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if m.Workspace.Raw != "./over" || !m.Workspace.Clone || !m.Workspace.Object {
		t.Errorf("workspace = %+v, want the later file's object form wholesale", m.Workspace)
	}
	if m.WorkspaceSource != "local.sbxenv.yaml" {
		t.Errorf("WorkspaceSource = %q, want the file that authored the winning value", m.WorkspaceSource)
	}
	if len(m.AdditionalWorkspaces) != 2 {
		t.Fatalf("additionalWorkspaces = %+v, want both files' entries concatenated", m.AdditionalWorkspaces)
	}
	if m.AdditionalWorkspaces[0].Path != "./one" || m.AdditionalWorkspaces[0].Source != "base.sbxenv.yaml" {
		t.Errorf("additionalWorkspaces[0] = %+v", m.AdditionalWorkspaces[0])
	}
	if m.AdditionalWorkspaces[1].Path != "./two" || m.AdditionalWorkspaces[1].Source != "local.sbxenv.yaml" {
		t.Errorf("additionalWorkspaces[1] = %+v", m.AdditionalWorkspaces[1])
	}

	// A later file that omits `workspace:` never clears an earlier one.
	silent := parseWorkspaceDoc(t, "schemaVersion: \"1\"\n")
	silent.Source = "silent.sbxenv.yaml"
	m2, err := Merge(base, silent)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if m2.Workspace.Raw != "./base" || m2.WorkspaceSource != "base.sbxenv.yaml" {
		t.Errorf("an omitted workspace cleared an earlier one: %+v from %q", m2.Workspace, m2.WorkspaceSource)
	}
}

func TestBuildTree_WorkspaceNodesAndInterpolation(t *testing.T) {
	doc := parseWorkspaceDoc(t, "schemaVersion: \"1\"\nworkspace: ${PIX_SRC}/app\nadditionalWorkspaces:\n  - path: ./shared\n  - path: ${PIX_DOCS}\n    readOnly: true\n")
	doc.Source = "a.sbxenv.yaml"
	m, err := Merge(doc)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	tree, err := BuildTree(m)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	if tree.Workspace == nil || tree.Workspace.KeyPath != "workspace" || tree.Workspace.Source != "a.sbxenv.yaml" {
		t.Fatalf("workspace node = %+v", tree.Workspace)
	}
	if len(tree.AdditionalWorkspaces) != 2 {
		t.Fatalf("additional workspace nodes = %+v", tree.AdditionalWorkspaces)
	}
	if tree.AdditionalWorkspaces[1].KeyPath != "additionalWorkspaces[1]" || !tree.AdditionalWorkspaces[1].ReadOnly {
		t.Errorf("additionalWorkspaces[1] node = %+v", tree.AdditionalWorkspaces[1])
	}
	got := map[string]string{}
	for _, in := range tree.Interpolations {
		got[in.KeyPath] = in.Var
	}
	if got["workspace"] != "PIX_SRC" {
		t.Errorf("workspace interpolation not surfaced: %v", tree.Interpolations)
	}
	if got["additionalWorkspaces[1]"] != "PIX_DOCS" {
		t.Errorf("additional workspace interpolation not surfaced: %v", tree.Interpolations)
	}
}

// TestComputeFingerprint_WorkspaceFacets pins the composed addresses drift
// is attributed by: they are the addresses of the rendered document, and a
// changed mount mode changes the fingerprint.
func TestComputeFingerprint_WorkspaceFacets(t *testing.T) {
	facts := RuntimeFacts{
		Document:                 &Document{SchemaVersion: SchemaVersionV1},
		PrimaryWorkspace:         WorkspaceFact{Path: "/w/project"},
		PersonalContextWorkspace: WorkspaceFact{Path: "/w/context"},
	}
	fp, err := ComputeFingerprint(facts, nil)
	if err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	if fp["workspace"] != "/w/project|clone=false" {
		t.Errorf("workspace facet = %q", fp["workspace"])
	}
	if fp["additionalWorkspaces[0]"] != "/w/context|readOnly=false" {
		t.Errorf("additionalWorkspaces[0] facet = %q", fp["additionalWorkspaces[0]"])
	}
	if _, stale := fp["mounts[0]"]; stale {
		t.Errorf("the old `mounts[i]` facet address is still emitted; it names no line of the rendered document")
	}

	facts.PersonalContextWorkspace.ReadOnly = true
	changed, err := ComputeFingerprint(facts, nil)
	if err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	if changed["additionalWorkspaces[0]"] == fp["additionalWorkspaces[0]"] {
		t.Errorf("flipping readOnly did not change the fingerprint")
	}
}

// TestRenderEffective_RefusesInexpressibleWorkspaceModes proves the two
// mount-mode facts upstream's schema cannot carry are REFUSED rather than
// dropped: silently dropping either one widens what the sandbox may write
// on the host.
func TestRenderEffective_RefusesInexpressibleWorkspaceModes(t *testing.T) {
	base := RuntimeFacts{Document: &Document{SchemaVersion: SchemaVersionV1}}

	ro := base
	ro.PrimaryWorkspace = WorkspaceFact{Path: "/w/project", ReadOnly: true}
	if _, err := RenderEffective(ro); !errors.Is(err, ErrReadOnlyPrimaryWorkspace) {
		t.Errorf("read-only primary workspace: err = %v, want ErrReadOnlyPrimaryWorkspace", err)
	}

	cloned := base
	cloned.PrimaryWorkspace = WorkspaceFact{Path: "/w/project"}
	cloned.AdditionalWorkspaces = []WorkspaceFact{{Path: "/w/other", Clone: true}}
	if _, err := RenderEffective(cloned); !errors.Is(err, ErrClonedAdditionalWorkspace) {
		t.Errorf("cloned additional workspace: err = %v, want ErrClonedAdditionalWorkspace", err)
	}
}

// TestRenderEffective_AuthoredReadOnlyMountWinsDedup pins the ordering
// rationale: authored entries render first, so a runtime duplicate can
// only ever be narrowed away by an authored read-only declaration, never
// widen it.
func TestRenderEffective_AuthoredReadOnlyMountWinsDedup(t *testing.T) {
	doc := parseWorkspaceDoc(t, "schemaVersion: \"1\"\nadditionalWorkspaces:\n  - path: /shared\n    readOnly: true\n")
	data, err := RenderEffective(RuntimeFacts{
		Document:             doc,
		PrimaryWorkspace:     WorkspaceFact{Path: "/w/project"},
		AdditionalWorkspaces: []WorkspaceFact{{Path: "/shared"}},
	})
	if err != nil {
		t.Fatalf("RenderEffective: %v", err)
	}
	if n := strings.Count(string(data), "path: /shared"); n != 1 {
		t.Fatalf("/shared rendered %d times, want exactly 1:\n%s", n, data)
	}
	if !strings.Contains(string(data), "path: /shared\n    readOnly: true") {
		t.Errorf("a runtime duplicate widened an authored read-only mount to read-write:\n%s", data)
	}
}
