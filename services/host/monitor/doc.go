// Package monitor is the host-side half of `pix monitor`: a debug wiretap
// that records a running sandbox's out-of-sandbox traffic (model
// requests/responses, tool + MCP calls, context/control events) to bounded,
// file-backed storage, and lets a reader tail/list it back out.
//
// It is pure library + net/http + os — no bubbletea, no SSE, no cmd/pix
// import — so it can be built and tested (`go test ./monitor/...`) on its
// own with real sockets and real files, no mocks. The wire protocol (event
// kinds, JSON field names) MUST match the in-VM emitter
// (extensions/monitor.ts) field-for-field; a kind this build doesn't
// recognize decodes to UnknownEvent rather than erroring (forward
// compatibility — see event.go).
//
// Shape:
//
//   - event.go: the Kind discriminator, Envelope + concrete event structs
//     (plus UnknownEvent for forward compat), Encode/Decode for the NDJSON
//     wire format, and the field-capping that bounds retained memory.
//   - redact.go: pattern-based secret redaction, applied to every event and
//     blob before either touches disk.
//   - paths.go: the filesystem safety layer (0700 dirs, 0600 files, no
//     symlink followed, no wire-supplied id used directly as a path
//     component) every write in this package goes through.
//   - store.go: the bounded, per-(sandboxId,sessionId) on-disk event
//     domain — Append/Tail/List.
//   - blobstore.go: the bounded, content-addressed on-disk blob domain —
//     Put/Get.
//   - ingest.go: the loopback-only HTTP ingest constructor (POST /ingest,
//     POST /blob, GET /healthz) that calls Store.Append/BlobStore.Put. NOT
//     wired into `pix-host serve` — see its doc comment.
package monitor
