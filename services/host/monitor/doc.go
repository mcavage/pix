// Package monitor is the host-side half of `pix monitor`: a debug wiretap
// that records a running sandbox's out-of-sandbox traffic (model
// requests/responses, tool + MCP calls, context/control events) to bounded,
// redacted, file-backed storage and reads it back out.
//
// Four files, one each: event.go is the wire contract with the in-VM tap
// (extensions/monitor.ts) — it MUST match field-for-field, and an
// unrecognized kind decodes to UnknownEvent rather than erroring. redact.go
// scrubs secret shapes from everything before it is written. store.go is the
// bounded NDJSON store plus its filesystem safety layer. ingest.go is the
// loopback-only HTTP server, and follow.go the reader; they share no state,
// only files. Nothing here imports cmd/pix or a TUI, so the whole package
// tests with real sockets and real files and no mocks.
package monitor
