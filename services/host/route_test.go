package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"pix/host/inference"
	"pix/host/routing"
)

// TestEstRunCost proves route show's real-dollar estimate: a metered model is
// priced from its per-Mtok rates via Model.CostFor over the reference workload,
// and a local (unmetered) model reads "free".
func TestEstRunCost(t *testing.T) {
	// sonnet-style prices: $3/Mtok in, $15/Mtok out.
	// refInputTokens=30_000, refOutputTokens=3_000 ->
	//   30_000/1e6*3 + 3_000/1e6*15 = 0.09 + 0.045 = 0.135
	metered := routing.Model{ID: "x/y", InputPerMTok: 3, OutputPerMTok: 15}
	if got, want := estRunCost(metered), "$0.1350"; got != want {
		t.Fatalf("metered est/run = %q, want %q", got, want)
	}

	local := routing.Model{ID: "ollama/z", Local: true, InputPerMTok: 3, OutputPerMTok: 15}
	if got, want := estRunCost(local), "free"; got != want {
		t.Fatalf("local est/run = %q, want %q", got, want)
	}
}

// TestModelStatus is the truth table the old boolean AVAIL column could not
// express. The bug it locks down: a catalog row's `available` bit means "Pix
// still routes to this model" (false = RETIRED), NOT "this host can call it",
// and rendering the first under a column a user reads as the second is what
// told a box with no OpenAI key that openai/gpt-5.6-sol was available.
func TestModelStatus(t *testing.T) {
	live := routing.Model{ID: "openai/x", Available: true}
	retired := routing.Model{ID: "anthropic/old", Available: false}

	for _, tc := range []struct {
		name    string
		catalog routing.Model
		wired   bool
		bound   bool
		want    string
	}{
		{"wired on this host", live, true, true, "wired"},
		{"in the catalog but not wired here", live, false, true, "unwired"},
		{"retired beats every host fact", retired, false, true, "retired"},
		{"retired even if a stale binding claims it", retired, true, true, "retired"},
		{"no host view: never claim callable", live, false, false, "in catalog"},
		{"no host view: still report retirement", retired, false, false, "retired"},
	} {
		if got := modelStatus(tc.catalog, tc.wired, tc.bound); got != tc.want {
			t.Errorf("%s: modelStatus = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestDroppedIntents proves compile reports what it left out. A dropped intent
// is the CORRECT outcome for a model this host cannot call — subagents inherit
// the parent model instead of failing at call time — but dropping it silently
// would read as "every intent routed", which is exactly how an unroutable
// OpenAI route survived in routing.json.
func TestDroppedIntents(t *testing.T) {
	pol := &routing.Policy{Intents: []routing.Intent{
		{Name: "code"}, {Name: "overlord"}, {Name: "review"},
	}}
	cr := routing.CompiledRouting{Routes: map[string]routing.CompiledRoute{
		"code":   {Model: "anthropic/claude-sonnet-5"},
		"review": {Model: "google/gemini-3.1-pro-preview"},
	}}
	got := droppedIntents(pol, cr)
	if len(got) != 1 || got[0] != "overlord" {
		t.Fatalf("droppedIntents = %v, want [overlord]", got)
	}
	if none := droppedIntents(&routing.Policy{}, cr); len(none) != 0 {
		t.Fatalf("no intents declared should drop nothing, got %v", none)
	}
}

// TestLoadView_BindingsAreTheAvailabilityAuthority is the regression test for
// the bug this whole file was rewritten for. With a config that wires ONLY
// anthropic, the router must not resolve any intent to a provider this host has
// no key for — not in `show`, not in `pick`, and above all not in the
// routing.json that `compile` writes and host-mode subagents read.
//
// Before the fix, every one of these subcommands loaded the shipped catalog and
// nothing else, so `overlord` (whose policy pins providers=[openai]) resolved to
// openai/gpt-5.6-sol on a box with no OpenAI key at all.
func TestLoadView_BindingsAreTheAvailabilityAuthority(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[inference]
  [inference.backends]
    [inference.backends.anthropic]
      driver = "native"
      auth = "1password"
      key_env = "ANTHROPIC_API_KEY"

  [[inference.models]]
    model = "anthropic/claude-opus-5"
    backend = "anthropic"
    upstream_id = "anthropic/claude-opus-5"
    available = true
    verified = true
    verified_by = "probe"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", cfgPath)

	v := loadView(nil)
	if !v.bound {
		t.Fatal("a config with bindings must narrow the registry; bound = false")
	}
	if got := v.wiredCount(); got != 1 {
		t.Fatalf("wiredCount = %d, want 1 (only the probed anthropic binding)", got)
	}
	for _, m := range v.reg.Models {
		if m.Provider == "openai" && m.Available {
			t.Fatalf("%s is available with no openai backend configured", m.ID)
		}
	}

	// The end-to-end claim: no intent may resolve to an unwired provider.
	for _, in := range v.pol.Intents {
		d := routing.Resolve(v.reg, v.sc, v.pol, in)
		m, ok := v.reg.Get(d.Model)
		if !ok {
			t.Fatalf("intent %q resolved to unknown model %q", in.Name, d.Model)
		}
		if !m.Available {
			// Resolve keeps an uncallable fallback as diagnostic output; compile is
			// what must drop it. Assert that contract rather than the resolve.
			cr := routing.MaterializeBindings(
				routing.Compile(v.reg, v.sc, v.pol, time.Time{}), inference.Bindings(v.cfg), "")
			if _, kept := cr.Routes[in.Name]; kept {
				t.Fatalf("intent %q compiled to uncallable model %q instead of being dropped", in.Name, d.Model)
			}
			continue
		}
		if m.Provider == "openai" {
			t.Fatalf("intent %q resolved to %q on a host with no openai key", in.Name, d.Model)
		}
	}

	// --catalog is the explicit escape hatch and must NOT be narrowed.
	if c := loadView([]string{"--catalog"}); c.bound || !c.catalogOnly {
		t.Fatalf("--catalog: bound = %v, catalogOnly = %v; want false, true", c.bound, c.catalogOnly)
	}
}
