package inference

import (
	"reflect"
	"strings"
	"testing"
)

// forbiddenHardwareShapeSubstrings are the shapes a scored routing candidate
// carries that a setup-only hardware fact never may: a price, a measured or
// seeded accuracy, a score, or a routing/intent binding. F12
// (architecture.md): the local Ollama hardware table (RAM, download size,
// declared context) must never grow one of these fields again.
var forbiddenHardwareShapeSubstrings = []string{
	"price", "cost", "usd", "mtok",
	"accuracy", "score",
	"intent", "routing", "objective", "fallback", "provider",
}

// TestLocalOllamaRungShapeHasNoScoredRoutingField reflects over
// LocalOllamaRung's field names (case-insensitively) and fails if any of them
// contain a forbidden substring. This is what stops a future edit from
// quietly turning the setup-only hardware table back into a routing
// candidate (the exact shape the deleted router's Model carried:
// InputPerMTok/OutputPerMTok, Provider, and — via Scorecard — accuracy).
func TestLocalOllamaRungShapeHasNoScoredRoutingField(t *testing.T) {
	typ := reflect.TypeOf(LocalOllamaRung{})
	for i := 0; i < typ.NumField(); i++ {
		name := strings.ToLower(typ.Field(i).Name)
		for _, bad := range forbiddenHardwareShapeSubstrings {
			if strings.Contains(name, bad) {
				t.Errorf("LocalOllamaRung.%s: field name contains forbidden substring %q (a setup-only hardware fact must never carry a price/accuracy/score/routing field)", typ.Field(i).Name, bad)
			}
		}
	}
}

// TestLocalOllamaRungsAreRAMDownloadContextOnly is the direct value-level
// companion: every shipped row has a real RAM/download/context fact and no
// row is a placeholder that would let a hollow struct pass the shape check
// above for the wrong reason.
func TestLocalOllamaRungsAreRAMDownloadContextOnly(t *testing.T) {
	rungs := LocalOllamaRungs()
	if len(rungs) == 0 {
		t.Fatal("LocalOllamaRungs() is empty; pix setup's local-model flow needs at least one rung")
	}
	for _, r := range rungs {
		if r.ID == "" || r.MinRAMGB <= 0 || r.DownloadGB <= 0 || r.ContextWindow <= 0 {
			t.Errorf("rung %+v is missing a RAM/download/context fact", r)
		}
	}
}
