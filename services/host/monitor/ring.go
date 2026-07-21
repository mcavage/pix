package monitor

import "sync"

// Ring is a fixed-capacity, thread-safe circular buffer of Events, bounded
// by BOTH event count (capacity) and cumulative estimated byte size
// (maxBytes — R2-7). The count cap alone doesn't stop a handful of
// maximally-sized events (e.g. context_event.detail near the old
// maxIngestLine) from retaining gigabytes; Add evicts the oldest event(s)
// whenever either bound would be exceeded. The Hub adds from HTTP handlers
// (possibly concurrently, one per in-flight /ingest request) while the TUI
// reads via Snapshot/Subscribe, so every method takes a mutex.
type Ring struct {
	mu       sync.Mutex
	buf      []Event
	sizes    []int // eventSize(buf[i]), 1:1 with buf; lets evictOldestLocked update totalBytes without re-encoding
	capacity int
	maxBytes int // 0 = no byte budget (count-only; see NewRing)

	start      int    // index of the oldest element in buf
	size       int    // number of valid elements currently stored
	totalBytes int    // sum of sizes[start..start+size) (mod capacity)
	dropped    uint64 // count of incoming events rejected outright (R3-3): sz > maxBytes alone
}

// NewRing creates a count-only Ring that retains at most capacity events,
// with no byte budget. A non-positive capacity is treated as 1 (a ring of
// zero makes no sense; we don't want NewRing to panic on a zero-value
// HubConfig field). Kept for callers/tests that only care about count; the
// Hub itself uses NewRingBytes (R2-7) to also cap total retained bytes.
func NewRing(capacity int) *Ring {
	return NewRingBytes(capacity, 0)
}

// NewRingBytes creates a Ring bounded by BOTH capacity (event count) and
// maxBytes (cumulative eventSize of all retained events). A non-positive
// maxBytes means no byte budget (equivalent to NewRing). Add evicts the
// oldest event(s) first whenever either bound would be exceeded by the
// incoming event, and STRICTLY enforces maxBytes (R3-3): an incoming event
// that alone exceeds maxBytes is dropped rather than retained over budget.
func NewRingBytes(capacity, maxBytes int) *Ring {
	if capacity <= 0 {
		capacity = 1
	}
	return &Ring{
		buf:      make([]Event, capacity),
		sizes:    make([]int, capacity),
		capacity: capacity,
		maxBytes: maxBytes,
	}
}

// Add appends e, evicting the oldest event(s) first if the ring is at
// capacity OR admitting e would exceed the byte budget (when maxBytes > 0).
// The byte budget is STRICT (R3-3): if e alone is larger than maxBytes,
// admitting it would leave Bytes() > maxBytes no matter how much else gets
// evicted (evicting everything else still isn't enough), so Add drops it
// outright instead — every retained event, and thus Bytes(), always stays
// within maxBytes. Add reports whether e was retained; a dropped event also
// increments the counter Dropped() exposes, so callers can tell a real drop
// apart from ordinary drop-oldest eviction. maxBytes == 0 means no byte
// budget at all (count-only; see NewRing) — an event is never dropped for
// size in that mode.
func (r *Ring) Add(e Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	sz := eventSize(e)

	if r.maxBytes > 0 && sz > r.maxBytes {
		r.dropped++
		return false
	}

	if r.size == r.capacity {
		r.evictOldestLocked()
	}
	if r.maxBytes > 0 {
		for r.size > 0 && r.totalBytes+sz > r.maxBytes {
			r.evictOldestLocked()
		}
	}

	idx := (r.start + r.size) % r.capacity
	r.buf[idx] = e
	r.sizes[idx] = sz
	r.totalBytes += sz
	r.size++
	return true
}

// evictOldestLocked discards the oldest retained event and advances start.
// Caller must hold mu. No-op if the ring is already empty.
func (r *Ring) evictOldestLocked() {
	if r.size == 0 {
		return
	}
	r.totalBytes -= r.sizes[r.start]
	r.buf[r.start] = nil // release the discarded Event for GC
	r.sizes[r.start] = 0
	r.start = (r.start + 1) % r.capacity
	r.size--
}

// Snapshot returns a copy of the currently retained events, oldest first.
func (r *Ring) Snapshot() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, r.size)
	for i := 0; i < r.size; i++ {
		out[i] = r.buf[(r.start+i)%r.capacity]
	}
	return out
}

// Len reports how many events are currently retained (<= capacity).
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.size
}

// Bytes reports the current cumulative estimated size (eventSize) of all
// retained events — the value Add keeps within maxBytes (R3-3: strictly,
// never over). Exposed mainly for tests/diagnostics; always 0 for a
// count-only Ring (maxBytes == 0) that happens to be empty, but still
// tracked even without a budget so callers can observe it.
func (r *Ring) Bytes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.totalBytes
}

// Dropped reports how many incoming events were rejected outright because a
// single event's estimated size exceeded the byte budget (maxBytes) on its
// own — R3-3. Distinct from ordinary eviction (which discards older
// RETAINED events to make room for a new one that fits): a dropped event
// never entered the ring at all.
func (r *Ring) Dropped() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}
