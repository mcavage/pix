package routing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ollamaOnlyRegistry is a pure-Ollama box: the cloud vendors are in the catalog
// but no binding made them available, which is exactly what
// RegistryForBindings produces on a machine whose only backend is Ollama.
func ollamaOnlyRegistry(t *testing.T) (*Registry, *Scorecard) {
	t.Helper()
	reg := embeddedDefaults[Registry](t, "models.json")
	sc := embeddedDefaults[Scorecard](t, "scorecard.json")
	for i := range reg.Models {
		if reg.Models[i].Provider != "ollama" || !reg.Models[i].Local {
			reg.Models[i].Available = false
		}
	}
	return reg, sc
}

// TestUnreachableVendorPreferenceRelaxesNothing is the inverse of the test it
// replaces. That one pinned the ladder dropping the provider allowlist FIRST —
// correct given a hard allowlist, but the allowlist itself was the mistake: a
// preference on a ladder of surrendered constraints turns "your vendor is not
// wired here" into "this route failed its constraints".
//
// Now an unreachable preference costs nothing. The accuracy floor — a real
// constraint — must still hold, which is what the old test was really
// protecting.
func TestUnreachableVendorPreferenceRelaxesNothing(t *testing.T) {
	reg, sc := ollamaOnlyRegistry(t)
	pol := &Policy{DefaultFallback: "anthropic/claude-sonnet-5"}
	in := Intent{
		Name: "overlord", TaskType: "reasoning", Objective: "accuracy",
		PreferProviders: []string{"openai"}, MinAccuracy: 0.50, Fallback: "openai/gpt-5.6-sol",
	}
	d := Resolve(reg, sc, pol, in)
	if !d.ConstraintsMet {
		t.Fatalf("an ollama-only box meets every CONSTRAINT this intent declares: %+v", d)
	}
	if len(d.Relaxed) != 0 {
		t.Fatalf("relaxed = %v, want nothing: a preference is not a constraint", d.Relaxed)
	}
	if d.PreferenceMet {
		t.Fatal("the openai preference could not be honored and must be reported as such")
	}
	if d.Chosen == nil || d.Chosen.Accuracy < in.MinAccuracy {
		t.Fatalf("the accuracy floor must still hold: %+v", d.Chosen)
	}
	if !strings.Contains(d.Reason, "preference did not apply") {
		t.Fatalf("reason must say the preference went unmet: %q", d.Reason)
	}
}

// TestBreadthOnOllamaOnlyBoxRespectsLatencyCeiling is today's cliff, exactly:
// every local model costs $0, so a cost objective tie-breaks on accuracy
// descending and a fanout of eight parallel children lands on the LARGEST local
// model. Latency is surrendered last precisely so this cannot happen.
func TestBreadthOnOllamaOnlyBoxRespectsLatencyCeiling(t *testing.T) {
	reg, sc := ollamaOnlyRegistry(t)
	pol := embeddedDefaults[Policy](t, "policy.json")
	var breadth Intent
	for _, in := range pol.Intents {
		if in.Name == "breadth" {
			breadth = in
		}
	}
	if breadth.Name == "" {
		t.Fatal("policy has no breadth intent; this guard is not testing anything")
	}
	d := Resolve(reg, sc, pol, breadth)
	if d.Model == "ollama/qwen3.5:35b" || d.Model == "ollama/qwen3.5:27b" {
		t.Fatalf("breadth landed on a large local rung (%s) \u2014 the cost tie broke on accuracy: %s", d.Model, d.Reason)
	}
	if d.Chosen == nil || (breadth.MaxLatencyMs > 0 && d.Chosen.LatencyMs > breadth.MaxLatencyMs) {
		t.Fatalf("breadth exceeded its latency ceiling: %+v (%s)", d.Chosen, d.Reason)
	}
	if containsString(d.Relaxed, "latency") {
		t.Fatalf("latency must be the LAST class surrendered, got relaxed=%v", d.Relaxed)
	}
}

// TestRelaxedRouteIsVisibleInReasonAndCompiledOutput: a relaxed route is a
// route the user did not ask for, so it must survive to routing.json. A
// silently degraded route is what made the original kimi-k3 incident invisible.
func TestRelaxedRouteIsVisibleInReasonAndCompiledOutput(t *testing.T) {
	reg, sc := ollamaOnlyRegistry(t)
	pol := embeddedDefaults[Policy](t, "policy.json")
	compiled := Compile(reg, sc, pol, time.Unix(0, 0))
	// `code` declares min_accuracy no local rung reaches, so it is genuinely
	// relaxed here. `overlord` is the CONTRAST: it merely prefers OpenAI, and a
	// vendor this box cannot reach must not be reported as a broken constraint —
	// that false alarm on the default install's default route is why the
	// allowlist became a preference.
	route, ok := compiled.Routes["code"]
	if !ok {
		t.Fatal("code route missing")
	}
	if len(route.Relaxed) == 0 {
		t.Fatalf("code (min_accuracy above every local rung) must be relaxed on an ollama-only box: %+v", route)
	}
	if o := compiled.Routes["overlord"]; len(o.Relaxed) != 0 || !o.ConstraintsMet {
		t.Fatalf("overlord only PREFERS openai; an unreachable preference must relax nothing: %+v", o)
	}
	path := filepath.Join(t.TempDir(), "routing.json")
	if err := WriteCompiled(path, compiled); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var back CompiledRouting
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Routes["code"].Relaxed) == 0 {
		t.Fatalf("relaxed did not survive to routing.json: %s", b)
	}
	if !strings.Contains(string(b), "\"relaxed\"") {
		t.Fatalf("routing.json must carry the relaxed field for machine readers: %s", b)
	}
}

// TestCompiledRoutingVersionIsUnchanged was the fleet guard (review nit N4)
// while extensions/subagents.ts still read routing.json and required an EXACT
// version match on it. E3.2 (docs/design/environments.md §7) moved every
// sandbox reader — extensions/inference.ts, extensions/subagents.ts, and
// extensions/ollama-bridge.ts — onto the additive `roster` field of the
// generated inference.json instead: there is no second generated routing
// artifact left for a sandbox extension to disagree with, so
// CompiledRoutingVersion no longer has a cross-language reader to stay pinned
// against. This guard now asserts the other half of that fact directly: no
// extension reads routing.json at all (Wave F deletes the artifact and this
// package; until then it simply has no sandbox-side reader left).
func TestCompiledRoutingVersionIsUnchanged(t *testing.T) {
	if CompiledRoutingVersion != 1 {
		t.Fatalf("CompiledRoutingVersion = %d, want 1 \u2014 bumping it is a no-op for sandbox readers now, but still a needless break for any external reader of the artifact before Wave F removes it", CompiledRoutingVersion)
	}
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "extensions", "subagents.ts"))
	if err != nil {
		t.Skipf("subagents.ts not readable from here: %v", err)
	}
	if strings.Contains(string(src), "routing.json") {
		t.Fatalf("extensions/subagents.ts must not read routing.json (docs/design/environments.md \u00a77); resolve the roster from inference.json instead")
	}
}

// TestUnfeasibleWithNoScoredCandidateStillFallsBack keeps the terminal
// diagnostic branch alive: with nothing scored+available at all, the ladder
// cannot help and the declared fallback is retained as diagnostic output.
func TestUnfeasibleWithNoScoredCandidateStillFallsBack(t *testing.T) {
	reg := &Registry{}
	sc := &Scorecard{}
	pol := &Policy{DefaultFallback: "anthropic/claude-sonnet-5"}
	d := Resolve(reg, sc, pol, Intent{Name: "code", TaskType: "code"})
	if d.Model != "anthropic/claude-sonnet-5" || d.ConstraintsMet {
		t.Fatalf("decision = %+v", d)
	}
	if len(d.Relaxed) != 0 {
		t.Fatalf("a fallback with no candidates relaxed nothing, got %v", d.Relaxed)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
