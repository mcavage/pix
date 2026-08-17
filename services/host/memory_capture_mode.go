// memory_capture_mode.go — the memory_capture admission gate: which of the
// two modes (explicit|experimental-auto) is live, plus the single persistent
// daily watcher-inference budget experimental-auto is metered against. See
// memObserve/memCapture (memory.go) for where this gates a capture.

package main

import (
	"os"
	"strings"
	"time"

	"pix/host/config"
)

// memCaptureMode reads the env var both `pix-host serve` (applyMemoryModelEnv)
// and the bare `pix-host memory` daemon (runMemory) translate cfg.MemoryCapture
// into. Anything but the opt-in value reads as explicit: unset/garbled must
// never silently turn automatic capture ON.
func memCaptureMode() string {
	if strings.TrimSpace(os.Getenv("MEMORY_CAPTURE_MODE")) == config.MemoryCaptureExperimentalAuto {
		return config.MemoryCaptureExperimentalAuto
	}
	return config.MemoryCaptureExplicit
}

// memWatcherDailyBudget: at most this many rows may be STORED per calendar
// day (UTC), counted from what's actually PERSISTED (memories rows with
// source='watcher'), never from how many times the watcher was CALLED. This
// is UX policy on an experimental feature, not a security boundary, so it
// needs no session ids, maps, or mutex: a SQL COUNT is the whole mechanism,
// and it survives a daemon restart exactly.
const memWatcherDailyBudget = 10

// watcherBudgetRemaining reports how many more rows may be stored today
// (UTC), never negative. Callers PEEK this before invoking the watcher
// (memObserve, then again in memCapture), and cap how many of a single
// watcher result's items may be stored to whatever remains.
//
// The COUNT deliberately does NOT filter deleted_at: a watcher row that was
// later `/forget`-ed (soft-deleted, deleted_at set) still STORED today and
// still cost a real watcher-model call plus a row write, so it keeps
// consuming today's budget exactly like a live one. This is not an oversight
// -- forgetting a row is feedback on content the caller no longer wants
// recalled, not a refund on capture volume; a caller could otherwise launder
// unlimited watcher writes by immediately forgetting each one. Only a NEW
// calendar day (UTC) resets the count, never a delete.
func (s *memStore) watcherBudgetRemaining() (int, error) {
	today := time.Now().UTC().Format("2006-01-02")
	var used int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM memories WHERE source = 'watcher' AND substr(created_at, 1, 10) = ?",
		today,
	).Scan(&used)
	if err != nil {
		return 0, err
	}
	remaining := memWatcherDailyBudget - used
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}
