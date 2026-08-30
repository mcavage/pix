package envinfo

import (
	"errors"
	"strings"
	"testing"
)

func TestRefuseReservedMCPNames(t *testing.T) {
	doc := &Document{Source: "/envs/work/.sbxenv.yaml"}
	doc.MCP.Servers = []MCPServer{{Name: "github"}, {Name: MCPMemoryName}, {Name: MCPSessionName}}

	err := RefuseReservedMCPNames(doc)
	var collision *ReservedMCPCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("an authored server named %q must refuse, got %v", MCPMemoryName, err)
	}
	if len(collision.Names) != 2 {
		t.Fatalf("both reserved collisions must be reported, got %v", collision.Names)
	}
	msg := err.Error()
	for _, want := range []string{MCPMemoryName, MCPSessionName, "/envs/work/.sbxenv.yaml", "rename it"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("refusal must name %q; got:\n%s", want, msg)
		}
	}

	ok := &Document{}
	ok.MCP.Servers = []MCPServer{{Name: "github"}, {Name: "pix-memory-of-mine"}}
	if err := RefuseReservedMCPNames(ok); err != nil {
		t.Fatalf("a non-reserved name must pass, got %v", err)
	}
	if err := RefuseReservedMCPNames(nil); err != nil {
		t.Fatalf("a nil document must pass, got %v", err)
	}
}

func TestRefuseUndefinedInterpolations(t *testing.T) {
	def := "fallback"
	tree := &Tree{Interpolations: []Interpolation{
		{Var: "SET_VAR", KeyPath: "env.A"},
		{Var: "HAS_DEFAULT", Default: &def, KeyPath: "env.B"},
		{Var: "MISSING_TWO", KeyPath: "mcp.servers[x].url"},
		{Var: "MISSING_ONE", KeyPath: "registries[y].host"},
		{Var: "MISSING_ONE", KeyPath: "env.C"}, // duplicate: reported once
	}}
	lookup := func(name string) (string, bool) {
		if name == "SET_VAR" {
			return "value", true
		}
		return "", false
	}

	err := RefuseUndefinedInterpolations(tree, lookup)
	var undef *UndefinedInterpolationError
	if !errors.As(err, &undef) {
		t.Fatalf("an undefined bare ${VAR} must refuse, got %v", err)
	}
	if len(undef.Vars) != 2 || undef.Vars[0] != "MISSING_ONE" || undef.Vars[1] != "MISSING_TWO" {
		t.Fatalf("undefined vars must be sorted and deduplicated, got %v", undef.Vars)
	}
	if undef.KeyPaths[0] != "registries[y].host" {
		t.Fatalf("each variable must carry its key path, got %v", undef.KeyPaths)
	}
	if !strings.Contains(err.Error(), "refusing to launch") {
		t.Fatalf("refusal must say it refuses rather than interpolating empty; got:\n%s", err)
	}

	// A default is authored intent, even an empty one.
	empty := ""
	only := &Tree{Interpolations: []Interpolation{{Var: "X", Default: &empty, KeyPath: "env.X"}}}
	if err := RefuseUndefinedInterpolations(only, lookup); err != nil {
		t.Fatalf("an authored default must pass, got %v", err)
	}
	// A nil lookup is the fail-closed direction.
	if err := RefuseUndefinedInterpolations(&Tree{Interpolations: []Interpolation{{Var: "Y", KeyPath: "env.Y"}}}, nil); err == nil {
		t.Fatalf("a nil lookup must refuse every bare reference")
	}
}

func TestClassifyDrifts(t *testing.T) {
	safe := []Drift{{ComposedKey: "sandboxOptions.template"}, {ComposedKey: "kits[0]"}, {ComposedKey: "kits[]"}}
	if got := ClassifyDrifts(safe); got != DriftRecreationSafe {
		t.Fatalf("pinned construction facets must be recreation-safe, got %s", got)
	}
	mixed := append(append([]Drift(nil), safe...), Drift{ComposedKey: "env.GITHUB_TOKEN"})
	if got := ClassifyDrifts(mixed); got != DriftSubstantive {
		t.Fatalf("one substantive facet must make the whole set substantive, got %s", got)
	}
	if got := ClassifyDrifts(nil); got != DriftNone {
		t.Fatalf("an empty set classifies as none, got %s", got)
	}
	if RecreationSafe(nil) {
		t.Fatalf("an empty drift set is nothing to recreate for, not something safe to recreate")
	}
	// A facet this package has never classified fails CLOSED.
	if ClassifyDrifts([]Drift{{ComposedKey: "somethingNew.invented"}}) != DriftSubstantive {
		t.Fatalf("an unclassified composed key must be substantive")
	}
}
