// Package monitor is the host-side half of `pi-stack monitor`: a live wiretap
// of a running sandbox's out-of-sandbox traffic (model requests/responses,
// tool + MCP calls, context/control events).
//
// It is pure library + net/http — no bubbletea, no cmd/pi-stack import — so it
// can be built and tested (`go test ./monitor/...`) on its own. The wire
// protocol (event kinds, JSON field names) is frozen by
// .pi-agent/deliver/monitor/architecture.md Section 2 and MUST match the
// in-VM emitter (extensions/monitor.ts) field-for-field.
//
// Shape:
//
//   - event.go: the Kind discriminator, Envelope + concrete event structs,
//     Encode/Decode helpers for the NDJSON wire format.
//   - ring.go: a bounded, thread-safe ring buffer of decoded events.
//   - blobcache.go: a bounded, thread-safe content-addressed blob cache.
//   - hub.go: the HTTP server (POST /ingest, POST /blob, GET /blob/{hash},
//     GET /stream, GET /healthz) plus the in-process Subscribe() seam used by
//     the TUI (Unit B) — no HTTP round-trip needed in the same binary.
package monitor
