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

// watcherUsedToday is the raw COUNT `watcherBudgetRemaining` and
// `rememberSourced`'s enforcement check both build on. The COUNT
// deliberately does NOT filter deleted_at: a watcher row that was later
// `/forget`-ed (soft-deleted, deleted_at set) still STORED today and still
// cost a real watcher-model call plus a row write, so it keeps consuming
// today's budget exactly like a live one. This is not an oversight --
// forgetting a row is feedback on content the caller no longer wants
// recalled, not a refund on capture volume; a caller could otherwise launder
// unlimited watcher writes by immediately forgetting each one. Only a NEW
// calendar day (UTC) resets the count, never a delete.
//
// Callers needing this atomic with a following write (rememberSourced) must
// hold s.mu themselves; this method takes no lock of its own so it composes
// under one already held instead of deadlocking.
func (s *memStore) watcherUsedToday() (int, error) {
	start, end := watcherDayBoundsUTC(time.Now())
	var used int
	err := s.db.QueryRow(
		"SELECT COUNT(*) FROM memories WHERE source = 'watcher' AND created_at >= ? AND created_at < ?",
		start, end,
	).Scan(&used)
	return used, err
}

// watcherDayBoundsUTC returns the [start, end) bounds of t's UTC calendar
// day, as plain "2006-01-02T15:04:05" strings — deliberately WITHOUT the
// trailing "Z" that memNowIso() always writes to created_at. Two things had
// to both be true for watcherUsedToday's substr(created_at,1,10)=? equality
// check to become a plain indexable range (created_at >= start AND
// created_at < end) that sqlite's query planner can serve entirely from
// idx_memories_source_created_at instead of a full table scan:
//
//  1. created_at is always UTC RFC3339Nano text, which sorts lexicographically
//     in time order — PROVIDED every value shares the same suffix shape. It
//     does not: Go's Nano formatter trims trailing zero fractional digits
//     (and omits the decimal point entirely for an exact whole second), so
//     two rows a few nanoseconds apart can have different-length strings.
//  2. Comparing a bound WITH a trailing "Z" (e.g. "...T00:00:00Z") against a
//     row that has sub-second digits before its own "Z" breaks at the first
//     differing byte: "...T00:00:00.5Z" sorts as LESS than "...T00:00:00Z"
//     because '.' (0x2E) < 'Z' (0x5A) — a row a half-second after midnight
//     would then compare as BEFORE midnight and fall out of today's window.
//
// Dropping the trailing "Z" from the bound fixes this: a bound is then a
// strict PREFIX of every created_at value at that exact second, and Go/SQL
// text comparison ranks a string strictly before any longer string sharing
// its full prefix. So "...T00:00:00" sorts before "...T00:00:00Z" (inclusive
// lower bound holds for the exact instant) and before "...T00:00:00.5Z" or
// any other fractional continuation (still holds arbitrarily close to the
// boundary), while a genuinely earlier or later calendar day still differs
// at the date digits, ahead of ever reaching the seconds field.
func watcherDayBoundsUTC(t time.Time) (start, end string) {
	t = t.UTC()
	dayStart := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	const noZ = "2006-01-02T15:04:05"
	return dayStart.Format(noZ), dayEnd.Format(noZ)
}

// watcherBudgetRemaining reports how many more rows may be stored today
// (UTC), never negative. Callers PEEK this before invoking the watcher
// (memObserve, then again in memCapture) to skip an already-exhausted day at
// zero inference cost. This peek is advisory, not the enforcement: it takes
// no lock, so a concurrent write can land between this read and whatever the
// caller does next. The actual cap is enforced exactly once, atomically
// under s.mu, at the point a watcher row would be INSERTed — see
// rememberSourced's budget check — so a racy peek here can only ever cause a
// wasted watcher call, never an over-stored day.
func (s *memStore) watcherBudgetRemaining() (int, error) {
	used, err := s.watcherUsedToday()
	if err != nil {
		return 0, err
	}
	remaining := memWatcherDailyBudget - used
	if remaining < 0 {
		remaining = 0
	}
	return remaining, nil
}
