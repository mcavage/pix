package routing

import (
	"math"
	"testing"
)

// TestMinRAMIncludesContextBudget is the S1 guard: min_ram_gb must price the KV
// cache of the context the rung DECLARES, not just the weights. A rung added
// with the old weights-only formula, or a context_window raised without
// repricing the gate, fails here.
func TestMinRAMIncludesContextBudget(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	seen := 0
	for _, m := range reg.Models {
		if !m.Local || !m.Available {
			continue
		}
		seen++
		want := math.Ceil(m.DownloadGB*1.15 + float64(m.ContextWindow)*m.KVGBPerTok + 1.0)
		if m.MinRAMGB != want {
			t.Errorf("%s: min_ram_gb = %g, want ceil(%g*1.15 + %d*%g + 1) = %g",
				m.ID, m.MinRAMGB, m.DownloadGB, m.ContextWindow, m.KVGBPerTok, want)
		}
	}
	if seen == 0 {
		t.Fatal("no available local rungs in the shipped catalog; this guard is not testing anything")
	}
}

// TestShippedLocalModelsDeclareMinRAM: a local rung with no gate would be
// offered to any machine, and a rung with no download size would be consented
// to without an honest size warning.
func TestShippedLocalModelsDeclareMinRAM(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	for _, m := range reg.Models {
		if !m.Local || !m.Available {
			continue
		}
		if m.MinRAMGB <= 0 {
			t.Errorf("%s is a local rung with no min_ram_gb; it would be offered to a machine that cannot run it", m.ID)
		}
		if m.DownloadGB <= 0 {
			t.Errorf("%s is a local rung with no download_gb; the pull prompt cannot warn about its size", m.ID)
		}
		if m.KVGBPerTok <= 0 {
			t.Errorf("%s is a local rung with no kv_gb_per_token; its gate is a magic constant", m.ID)
		}
	}
}

// TestLocalRungContextWindowIsBudgetedNot256K is S1's other half: the catalog
// must not advertise a context the machine cannot hold, because
// compileInferenceRuntime copies context_window straight into the runtime
// manifest the sandbox plans against. 256K is the architecture's maximum, not
// the context we sized RAM for.
func TestLocalRungContextWindowIsBudgetedNot256K(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	for _, m := range reg.Models {
		if !m.Local || !m.Available {
			continue
		}
		if m.ContextWindow > 65536 {
			t.Errorf("%s declares context_window %d; a local rung advertises the RAM-BUDGETED context (its min_ram_gb term), not the architecture maximum", m.ID, m.ContextWindow)
		}
		kvGB := float64(m.ContextWindow) * m.KVGBPerTok
		if kvGB > m.MinRAMGB {
			t.Errorf("%s: the declared context alone (%.1f GB of KV) exceeds its own gate (%g GB)", m.ID, kvGB, m.MinRAMGB)
		}
	}
}

// TestModelWithoutMinRAMNeverFits keeps the degradation conservative: an
// undeclared requirement is not a small one. A user's ~/.pix/routing/models.json
// override that predates the gate must not have its local models offered.
func TestModelWithoutMinRAMNeverFits(t *testing.T) {
	m := Model{ID: "ollama/mystery", Local: true, Available: true}
	if m.FitsMemory(1024) {
		t.Fatal("a local model with no declared min_ram_gb must never fit, even on a huge machine")
	}
	if (Model{MinRAMGB: 10}).FitsMemory(9.99) {
		t.Fatal("min_ram_gb is a floor on USABLE memory; 9.99 does not satisfy 10")
	}
	if !(Model{MinRAMGB: 10}).FitsMemory(10) {
		t.Fatal("min_ram_gb <= usable must fit exactly at the boundary")
	}
}

// TestLocalRungsAreLargestFirst pins the ordering both the offer filter and the
// serialized setup probe depend on: if a probe budget runs out, it must not run
// out on the rung the roster will actually use.
func TestLocalRungsAreLargestFirst(t *testing.T) {
	reg := embeddedDefaults[Registry](t, "models.json")
	rungs := LocalRungs(reg)
	if len(rungs) < 4 {
		t.Fatalf("expected the full local ladder, got %d rungs", len(rungs))
	}
	for i := 1; i < len(rungs); i++ {
		if rungs[i-1].MinRAMGB < rungs[i].MinRAMGB {
			t.Fatalf("LocalRungs is not largest-first: %s (%g) before %s (%g)",
				rungs[i-1].ID, rungs[i-1].MinRAMGB, rungs[i].ID, rungs[i].MinRAMGB)
		}
	}
	for _, m := range rungs {
		if !m.Local || !m.Available {
			t.Fatalf("LocalRungs returned a non-local or unavailable model: %+v", m)
		}
	}
}
