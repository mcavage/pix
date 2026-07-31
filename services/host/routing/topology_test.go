package routing

import (
	"fmt"
	"testing"
	"time"
)

func TestAvailabilityTopologyMatrix(t *testing.T) {
	taskTypes := []string{"code", "reasoning", "search", "qa"}
	models := []Model{
		{ID: "lab/alpha", Provider: "lab", Family: "alpha", Available: true},
		{ID: "lab/beta", Provider: "lab", Family: "beta", Available: true},
		{ID: "other/gamma", Provider: "other", Family: "gamma", Available: true},
		{ID: "third/delta", Provider: "third", Family: "delta", Available: true},
	}
	var scores []Score
	for mi, m := range models {
		for _, task := range taskTypes {
			scores = append(scores, Score{Model: m.ID, TaskType: task, Accuracy: .7 + float64(mi)/20, CostUSD: .1, LatencyMsP50: 1000, Source: "test"})
		}
	}
	pol := &Policy{DefaultFallback: models[0].ID}
	for _, task := range taskTypes {
		pol.Intents = append(pol.Intents, Intent{Name: task, TaskType: task, Objective: "accuracy", Providers: []string{"provider-that-may-be-absent"}})
	}
	for count := 0; count <= len(models); count++ {
		t.Run(fmt.Sprintf("%d-models", count), func(t *testing.T) {
			var bindings []Binding
			for i := 0; i < count; i++ {
				bindings = append(bindings, Binding{Model: models[i].ID, Backend: "gateway", UpstreamID: fmt.Sprintf("m%d", i), Available: true})
			}
			filtered := RegistryForBindings(&Registry{Models: models}, bindings, "")
			got := MaterializeBindings(Compile(filtered, &Scorecard{Scores: scores}, pol, time.Unix(1, 0)), bindings, "")
			if count == 0 {
				if len(got.Routes) != 0 {
					t.Fatalf("no-model topology produced routes: %+v", got.Routes)
				}
				return
			}
			if len(got.Routes) != len(taskTypes) {
				t.Fatalf("%d-model topology has %d routes, want %d", count, len(got.Routes), len(taskTypes))
			}
			for name, route := range got.Routes {
				if route.Model == "" {
					t.Fatalf("intent %s has no model", name)
				}
			}
		})
	}
}

func TestFullShippedCatalogTopology(t *testing.T) {
	t.Setenv("ROUTING_DIR", t.TempDir())
	reg, err := LoadRegistry()
	if err != nil {
		t.Fatal(err)
	}
	sc, err := LoadScorecard()
	if err != nil {
		t.Fatal(err)
	}
	pol, err := LoadPolicy()
	if err != nil {
		t.Fatal(err)
	}
	var bindings []Binding
	wantModels := 0
	for _, m := range reg.Models {
		if !m.Available {
			continue
		}
		wantModels++
		bindings = append(bindings, Binding{Model: m.ID, Backend: m.Provider, UpstreamID: m.ID, Available: true})
	}
	filtered := RegistryForBindings(reg, bindings, "")
	got := MaterializeBindings(Compile(filtered, sc, pol, time.Unix(1, 0)), bindings, "")
	if wantModels < 4 {
		t.Fatalf("shipped full topology unexpectedly small: %d models", wantModels)
	}
	if len(got.Routes) != len(pol.Intents) {
		t.Fatalf("full topology compiled %d/%d intents", len(got.Routes), len(pol.Intents))
	}
}
