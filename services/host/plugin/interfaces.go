package plugin

// The three capability interfaces below are derived from the real host services.
// They deliberately use plain Go structs (no context.Context, no maps of any) so
// their arguments and results are trivially gob-compatible for the net/rpc
// transport. When the transport is upgraded to gRPC, context can be threaded
// back in per the arch doc.

// --- MemoryStore -------------------------------------------------------------
//
// Mirrors the JSON-RPC surface of ../memory.go (memoryMux()'s `methods` map).
// This process keeps the JSON-RPC front-end on :11435 and turns each method
// into a typed call on this interface.

type MemoryStore interface {
	Remember(RememberReq) (RememberResp, error)
	Recall(RecallReq) (RecallResp, error)
	Forget(ForgetReq) (ForgetResp, error)
	Synthesize(SynthesizeReq) (SynthesizeResp, error)
	Observe(ObserveReq) (ObserveResp, error)
	Stats(profile string) (Stats, error)
	Health() (Health, error)
}

// RememberReq mirrors memory.go's rememberInput / rememberFromParams.
//
// Durability, TTLDays, and Reward are all gone from the write path: every
// row this binary writes is durable (the perishable/TTL behavior Durability/
// TTLDays configured was removed along with the watcher's event channel),
// and reward is no longer accepted as write-side input at all (it was never
// read back into recall's score even before this). The `reward` column
// stays in the schema, inert, defaulting to 0 on every row.
type RememberReq struct {
	Content    string
	Kind       string
	Source     string
	Project    string
	Profile    string
	HasProject bool
	Confidence float64
	Tags       []string
	Dedupe     float64
	HasDedupe  bool
}

// RememberResp mirrors memory.go remember()'s {id, reaffirmed}.
type RememberResp struct {
	ID         string
	Reaffirmed bool
}

// RecallReq mirrors the recall() params.
type RecallReq struct {
	Query      string
	Limit      int
	CharBudget int
	Kind       string
	Project    string
	Profile    string
}

// Hit mirrors a scoredHit as surfaced by the recall JSON-RPC result.
type Hit struct {
	ID         string
	Content    string
	Score      float64
	Kind       string
	Durability string
	Project    string
	CreatedAt  string // RFC3339; the recall extension renders it
}

// RecallResp wraps the hit list ({"hits": [...]}).
type RecallResp struct {
	Hits []Hit
}

// ForgetReq / ForgetResp mirror forget(idOrPrefix) -> {ok}.
type ForgetReq struct {
	ID      string
	Profile string
}

type ForgetResp struct {
	OK bool
}

// SynthesizeReq / SynthesizeResp mirror synthesize(threshold) -> {merged}.
// The response used to also carry an "expired" count from a background
// TTL-expiry sweep; the sweep was deleted and the field had no remaining
// caller, so it was dropped rather than pinned at a permanent 0.
type SynthesizeReq struct {
	Threshold float64
}

type SynthesizeResp struct {
	Merged int
}

// ObserveReq / ObserveResp mirror the observe method (async memCapture).
type ObserveReq struct {
	User       string
	Project    string
	Profile    string
	HasProject bool
}

type ObserveResp struct {
	Accepted bool
	Reason   string
}

// Stats mirrors stats().
type Stats struct {
	Active     int
	Durable    int
	Perishable int
	Facts      int
	Learnings  int
	Deleted    int
}

// Health mirrors the health method ({ok, vector, capture, watcherModel}).
// Vector/Capture are tri-state: nil means "not yet exercised" (no real
// embed/watcher attempt has happened since the process started), so a
// pointer is used rather than a bool that could only ever say true/false and
// would have to pick one of them to mean "don't actually know yet" — the
// false-healthy gap this type closes. See embedHealthState/watcherHealthState
// in the host's memembed.go/memory.go.
type Health struct {
	OK            bool
	Vector        *bool
	Capture       *bool
	WatcherModel  string
	CaptureReason string // explains a false Capture (JSON-RPC `captureReason`)
}
