package main

import (
	"testing"

	"pi-stack/host/routing"
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
