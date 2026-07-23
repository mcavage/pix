package monitor

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

func turnStart(seq uint64, model string) Event {
	return TurnStart{
		env:   env{Kind: KindTurnStart, Seq: seq},
		Model: model,
	}
}

func TestRingSnapshotOrderNoOverflow(t *testing.T) {
	r := NewRing(5)
	for i := 0; i < 3; i++ {
		r.Add(turnStart(uint64(i), fmt.Sprintf("m%d", i)))
	}
	if got := r.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot() len = %d, want 3", len(snap))
	}
	for i, e := range snap {
		ts := e.(TurnStart)
		if ts.Envelope().Seq != uint64(i) {
			t.Fatalf("snap[%d].Seq = %d, want %d (order not oldest->newest)", i, ts.Envelope().Seq, i)
		}
	}
}

func TestRingEvictsOldestOnOverflow(t *testing.T) {
	r := NewRing(3)
	for i := 0; i < 5; i++ { // 0,1,2,3,4 into a ring of 3 -> keeps 2,3,4
		r.Add(turnStart(uint64(i), "m"))
	}
	if got := r.Len(); got != 3 {
		t.Fatalf("Len() = %d, want 3", got)
	}
	snap := r.Snapshot()
	want := []uint64{2, 3, 4}
	for i, e := range snap {
		got := e.(TurnStart).Envelope().Seq
		if got != want[i] {
			t.Fatalf("snap[%d].Seq = %d, want %d", i, got, want[i])
		}
	}
}

func TestRingZeroCapacityTreatedAsOne(t *testing.T) {
	r := NewRing(0)
	r.Add(turnStart(1, "m"))
	r.Add(turnStart(2, "m"))
	if got := r.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].(TurnStart).Envelope().Seq != 2 {
		t.Fatalf("Snapshot() = %+v, want the last-added event only", snap)
	}
}

func TestRingSnapshotIsACopy(t *testing.T) {
	r := NewRing(2)
	r.Add(turnStart(1, "m"))
	snap := r.Snapshot()
	r.Add(turnStart(2, "m"))
	if len(snap) != 1 {
		t.Fatalf("earlier Snapshot() mutated after later Add(); len=%d", len(snap))
	}
}

// bigContextEvent builds a valid ContextEvent whose Detail is n bytes,
// letting tests exercise Ring's byte budget (R2-7) with events that are
// individually large but well under maxFieldBytes/maxIngestLine so they
// aren't truncated or rejected before ever reaching the ring.
func bigContextEvent(seq uint64, n int) Event {
	return ContextEvent{
		env:     env{Kind: KindContextEvent, Seq: seq},
		CtxKind: "compaction",
		Detail:  strings.Repeat("x", n),
	}
}

// TestRingByteBudgetEvictsOldestFirst is R2-7: a Ring with a byte budget
// (maxBytes) must evict oldest-first on BYTES, not just count, so a flood of
// large valid events (e.g. context_event.detail near the old 8MB
// maxIngestLine) can never retain more than maxBytes total regardless of how
// far under the count cap it is.
func TestRingByteBudgetEvictsOldestFirst(t *testing.T) {
	const (
		eventBytes = 64 << 10  // 64KB Detail per event, well under maxFieldBytes
		maxBytes   = 512 << 10 // budget for ~8 events at eventBytes each
		capacity   = 2000      // count cap intentionally never binds in this test
		n          = 40        // 40 * 64KB = 2.5MB >> maxBytes: budget must bind first
	)
	r := NewRingBytes(capacity, maxBytes)
	for i := 0; i < n; i++ {
		r.Add(bigContextEvent(uint64(i), eventBytes))
	}

	if got := r.Bytes(); got > maxBytes {
		t.Fatalf("Bytes() = %d, want <= maxBytes (%d)", got, maxBytes)
	}
	if got := r.Len(); got >= capacity {
		t.Fatalf("Len() = %d, want well under capacity (%d) — the byte budget, not the count cap, should have evicted here", got, capacity)
	}

	// Whatever survived must be the NEWEST contiguous run (oldest evicted
	// first): the last event (seq n-1) must be present, and snapshot order
	// must be strictly increasing with no gaps.
	snap := r.Snapshot()
	if len(snap) == 0 {
		t.Fatalf("Snapshot() empty, want at least the newest event retained")
	}
	if last := snap[len(snap)-1].Envelope().Seq; last != uint64(n-1) {
		t.Fatalf("snap[last].Seq = %d, want %d (the newest event must survive)", last, n-1)
	}
	for i := 1; i < len(snap); i++ {
		prev := snap[i-1].Envelope().Seq
		cur := snap[i].Envelope().Seq
		if cur != prev+1 {
			t.Fatalf("snap[%d].Seq = %d, snap[%d].Seq = %d: not contiguous (oldest-evicted-first violated or a gap)", i-1, prev, i, cur)
		}
	}
}

// TestRingByteBudgetOversizedEventDropped is R3-3: the byte budget is
// STRICT — one event bigger than maxBytes on its own must be DROPPED, not
// retained over budget, so Bytes() never exceeds maxBytes regardless of how
// large a single incoming event is.
func TestRingByteBudgetOversizedEventDropped(t *testing.T) {
	const maxBytes = 1 << 10 // 1KB budget
	r := NewRingBytes(10, maxBytes)
	ok := r.Add(bigContextEvent(1, 4<<10)) // 4KB event, over budget alone

	if ok {
		t.Fatalf("Add() = true, want false (oversized event must be dropped)")
	}
	if got := r.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0 (dropped event must not be retained)", got)
	}
	if got := r.Bytes(); got > maxBytes {
		t.Fatalf("Bytes() = %d, want <= maxBytes (%d)", got, maxBytes)
	}
	if got := r.Dropped(); got != 1 {
		t.Fatalf("Dropped() = %d, want 1", got)
	}

	// A normal-sized event afterward must still be accepted and retained
	// normally — the strict budget only rejects the oversized event itself,
	// it doesn't wedge the ring.
	if ok := r.Add(bigContextEvent(2, 100)); !ok {
		t.Fatalf("Add() = false, want true for a normal-sized event")
	}
	if got := r.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1", got)
	}
	if got := r.Bytes(); got > maxBytes {
		t.Fatalf("Bytes() = %d, want <= maxBytes (%d)", got, maxBytes)
	}
}

// TestRingConcurrentAddAndSnapshot exercises Ring under -race: the Hub adds
// from HTTP handlers while the TUI reads via Snapshot/Len concurrently.
func TestRingConcurrentAddAndSnapshot(t *testing.T) {
	r := NewRing(50)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r.Add(turnStart(uint64(i), "m"))
		}(i)
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Snapshot()
			_ = r.Len()
		}()
	}
	wg.Wait()
	if got := r.Len(); got != 20 {
		t.Fatalf("Len() = %d, want 20", got)
	}
}
