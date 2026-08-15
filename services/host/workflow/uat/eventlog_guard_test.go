package uat

import (
	"path/filepath"
	"strings"
	"testing"
)

// An event log that could not be opened must FAIL, not fault. NewEventLog
// dropped NewEventStore's error and returned a log with a nil store, so the
// first Append dereferenced nil and took the process down with SIGSEGV — and
// then did it again from executeAsync's deferred handler, which appends too, so
// the recover that exists to record the failure became a second identical
// panic. Every Append call site discards the error (`_ =`) on purpose: the log
// is best-effort evidence, and best-effort must mean "returns an error", never
// "kills the run it was supposed to describe".
func TestEventLog_UnopenableLogFailsInsteadOfFaulting(t *testing.T) {
	// Parent does not exist, which is what a run directory removed underneath an
	// async run looks like — a t.TempDir() cleanup racing a Submit goroutine.
	missing := filepath.Join(t.TempDir(), "gone", "events.log")
	log := NewEventLog(missing)

	err := log.Append(Event{Type: EventRunDone, State: "pass"})
	if err == nil {
		t.Fatal("Append on an unopenable log must return an error")
	}
	if !strings.Contains(err.Error(), "event log unavailable") {
		t.Errorf("the error must name the cause, got %v", err)
	}

	if _, err := log.ReadSince(0); err == nil {
		t.Error("ReadSince on an unopenable log must return an error")
	}

	// The deferred-handler shape: appending again must stay an error, never a
	// panic, or a run failure is replaced by a crash.
	if err := log.Append(Event{Type: EventStatus, State: "cleanup_fail"}); err == nil {
		t.Error("a second Append must also fail cleanly")
	}
}
