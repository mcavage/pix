// Package positive is a planted fixture: every package-level var in it IS a
// function-valued seam and must show up in funcValuedGlobalHits. It is a
// standalone fixture package, never wired into pkgLayer, so it never
// participates in the real architecture guard itself — see
// TestFuncValuedGlobalHits_PlantedFixtures in wiring_fixtures_test.go, which
// points the guard's own detector at it directly.
package positive

// realFunc is a plain top-level function declared in this fixture package.
// vars.go's VarBoundToFunc references it bare (no call) — the "identifier
// bound to a func" shape the guard must catch, resolved across files within
// the same package directory.
func realFunc(x int) int { return x + 1 }
