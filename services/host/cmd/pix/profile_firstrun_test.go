package main

import (
	"reflect"
	"testing"
)

func TestRunBareInvocation_FirstRunDispatchesSetupThenRun(t *testing.T) {
	var calls [][]string
	code := runBareInvocation(true, true, func(args []string) int {
		calls = append(calls, append([]string(nil), args...))
		return 0
	})
	want := [][]string{{"setup"}, {"run"}}
	if code != 0 || !reflect.DeepEqual(calls, want) {
		t.Fatalf("runBareInvocation = code %d calls %v, want 0 and %v", code, calls, want)
	}
}

func TestPlanBareInvocation_FirstRunSetsUpThenRuns(t *testing.T) {
	calls := 0
	args, code, stop := planBareInvocation(true, true, func() int {
		calls++
		return 0
	})
	if calls != 1 {
		t.Fatalf("setup calls = %d, want 1", calls)
	}
	if code != 0 || stop {
		t.Fatalf("plan = (args=%v code=%d stop=%v), want a continuing run", args, code, stop)
	}
	if !reflect.DeepEqual(args, []string{"run"}) {
		t.Fatalf("args = %v, want [run]", args)
	}
}

func TestPlanBareInvocation_SetupFailureStops(t *testing.T) {
	args, code, stop := planBareInvocation(true, true, func() int { return 7 })
	if len(args) != 0 || code != 7 || !stop {
		t.Fatalf("plan = (args=%v code=%d stop=%v), want setup failure 7", args, code, stop)
	}
}

func TestPlanBareInvocation_ExistingConfigSkipsSetup(t *testing.T) {
	calls := 0
	args, code, stop := planBareInvocation(true, false, func() int { calls++; return 0 })
	if calls != 0 || code != 0 || stop || !reflect.DeepEqual(args, []string{"run"}) {
		t.Fatalf("plan = (args=%v code=%d stop=%v calls=%d), want ordinary run", args, code, stop, calls)
	}
}

func TestPlanBareInvocation_NonInteractiveNeverSetsUp(t *testing.T) {
	calls := 0
	args, code, stop := planBareInvocation(false, true, func() int { calls++; return 0 })
	if calls != 0 || code != 0 || stop || !reflect.DeepEqual(args, []string{"ls"}) {
		t.Fatalf("plan = (args=%v code=%d stop=%v calls=%d), want read-only ls", args, code, stop, calls)
	}
}
