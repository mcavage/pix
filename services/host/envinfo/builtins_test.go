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

const (
	scopedMemoryName  = "pix-memory-0123456789abcdef"
	scopedSessionName = "pix-session-0123456789abcdef"
)

// TestIsReservedMCPName_ReservesBareLegacyAndWholeScopedFamily is the Wave
// B coexistence requirement: an authored environment must never be able to
// shadow a built-in by guessing ANY validly-shaped scoped name, not just
// this PIX_HOME's own current one -- the whole family is reserved, not one
// literal string per stack.
func TestIsReservedMCPName_ReservesBareLegacyAndWholeScopedFamily(t *testing.T) {
	reserved := []string{
		MCPMemoryName, MCPSessionName,
		scopedMemoryName, scopedSessionName,
		"pix-memory-fedcba9876543210", // a DIFFERENT stack's own scoped name
		"pix-session-fedcba9876543210",
	}
	for _, name := range reserved {
		if !IsReservedMCPName(name) {
			t.Errorf("IsReservedMCPName(%q) = false, want true", name)
		}
	}
	notReserved := []string{
		"github", "pix-memory-of-mine", "pix-memory-", "pix-memory-xyz",
		"pix-memory-0123456789abcdefg", // 17 chars, not 16
		"PIX-MEMORY-0123456789abcdef",  // wrong case
	}
	for _, name := range notReserved {
		if IsReservedMCPName(name) {
			t.Errorf("IsReservedMCPName(%q) = true, want false", name)
		}
	}
}

// TestRefuseReservedMCPNames_CatchesAnyScopedFamilyMember proves the
// refusal itself (not just the predicate) rejects an authored name shaped
// like ANY stack's built-in, not only this host's current one.
func TestRefuseReservedMCPNames_CatchesAnyScopedFamilyMember(t *testing.T) {
	doc := &Document{Source: "/envs/work/.sbxenv.yaml"}
	doc.MCP.Servers = []MCPServer{{Name: "github"}, {Name: "pix-memory-fedcba9876543210"}}
	err := RefuseReservedMCPNames(doc)
	var collision *ReservedMCPCollisionError
	if !errors.As(err, &collision) {
		t.Fatalf("an authored scoped-family name must refuse, got %v", err)
	}
	if len(collision.Names) != 1 || collision.Names[0] != "pix-memory-fedcba9876543210" {
		t.Fatalf("expected exactly the scoped-family collision, got %v", collision.Names)
	}
}

func TestBuiltinMCPServers(t *testing.T) {
	out := BuiltinMCPServers(BuiltinMCPFacts{
		MemoryName:     scopedMemoryName,
		MemoryURL:      "http://127.0.0.1:18080/mcp",
		SessionName:    scopedSessionName,
		SessionCommand: "/usr/local/bin/pix",
		SessionArgs:    []string{"mcp-session"},
	})
	if len(out) != 2 {
		t.Fatalf("expected both built-ins, got %+v", out)
	}
	if out[0].Name != scopedMemoryName || out[0].URL != "http://127.0.0.1:18080/mcp" {
		t.Fatalf("pix-memory fact wrong: %+v", out[0])
	}
	if out[1].Name != scopedSessionName || out[1].Command != "/usr/local/bin/pix" || len(out[1].Args) != 1 || out[1].Args[0] != "mcp-session" {
		t.Fatalf("pix-session fact wrong: %+v", out[1])
	}

	// A caller that resolved neither fact gets no entries at all -- never a
	// fabricated one.
	if got := BuiltinMCPServers(BuiltinMCPFacts{}); got != nil {
		t.Fatalf("an empty BuiltinMCPFacts must render no built-ins, got %+v", got)
	}

	// Only the resolved half renders.
	memOnly := BuiltinMCPServers(BuiltinMCPFacts{MemoryName: scopedMemoryName, MemoryURL: "http://x"})
	if len(memOnly) != 1 || memOnly[0].Name != scopedMemoryName {
		t.Fatalf("memory-only facts must render only pix-memory, got %+v", memOnly)
	}

	// A resolved URL/Command with NO paired scoped Name must render nothing:
	// there is no global bare-name fallback left to reach for.
	if got := BuiltinMCPServers(BuiltinMCPFacts{MemoryURL: "http://x"}); got != nil {
		t.Fatalf("a MemoryURL with no MemoryName must render nothing, got %+v", got)
	}
	if got := BuiltinMCPServers(BuiltinMCPFacts{SessionCommand: "/bin/pix"}); got != nil {
		t.Fatalf("a SessionCommand with no SessionName must render nothing, got %+v", got)
	}

	// Mutating the caller's SessionArgs after the call must not reach the
	// rendered fact's own copy.
	args := []string{"mcp-session"}
	fact := BuiltinMCPServers(BuiltinMCPFacts{SessionName: scopedSessionName, SessionCommand: "/bin/pix", SessionArgs: args})
	args[0] = "clobbered"
	if fact[0].Args[0] != "mcp-session" {
		t.Fatalf("SessionArgs must be copied, not aliased: %+v", fact[0].Args)
	}
}

func TestWithBuiltinMCPServers(t *testing.T) {
	existing := []MCPWrapperFact{
		{Name: "github"},
		// A legacy static-MCP NAME-only placeholder under a reserved name --
		// the only way this could ever have reached here (see the function's
		// own doc comment) -- must be superseded, not folded in beside the
		// real fact. Both the bare legacy name AND a differently-scoped name
		// (a stale placeholder from before this stack's own id, or another
		// stack's) must be dropped the same way.
		{Name: MCPMemoryName},
		{Name: "pix-memory-fedcba9876543210"},
	}
	got := WithBuiltinMCPServers(existing, BuiltinMCPFacts{MemoryName: scopedMemoryName, MemoryURL: "http://127.0.0.1:18080/mcp"})
	if len(got) != 2 {
		t.Fatalf("expected github + the real pix-memory fact, got %+v", got)
	}
	if got[0].Name != "github" {
		t.Fatalf("non-reserved entries must keep their place, got %+v", got)
	}
	if got[1].Name != scopedMemoryName || got[1].URL != "http://127.0.0.1:18080/mcp" {
		t.Fatalf("the real pix-memory fact must win over the placeholder, got %+v", got[1])
	}

	// No existing servers and no resolved builtins: nil, never an empty
	// non-nil slice a caller could mistake for "servers explicitly cleared".
	if got := WithBuiltinMCPServers(nil, BuiltinMCPFacts{}); got != nil {
		t.Fatalf("expected nil, got %+v", got)
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
