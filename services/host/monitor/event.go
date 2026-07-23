package monitor

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Kind discriminates the flat wire event shape. See architecture.md Section
// 2.3 — the wire encoding is a flat JSON object with a "kind" field, not a
// nested {envelope,data}, so Go can unmarshal into one concrete struct per
// kind after a cheap first pass on "kind".
type Kind string

const (
	KindTurnStart        Kind = "turn_start"
	KindProviderRequest  Kind = "provider_request"
	KindProviderResponse Kind = "provider_response"
	KindToolStart        Kind = "tool_start"
	KindToolEnd          Kind = "tool_end"
	KindContextEvent     Kind = "context_event"
	KindBlob             Kind = "blob"
)

// Envelope holds the common fields present on every event.
type Envelope struct {
	Kind      Kind   `json:"kind"`
	SandboxID string `json:"sandboxId"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	Seq       uint64 `json:"seq"`
	TS        int64  `json:"ts"`
}

// env is an alias for Envelope used ONLY as the anonymous-embedding field name
// in each concrete event struct below. Anonymous embedding is what gives us
// automatic encoding/json flattening (the envelope fields land at the top
// level of the wire JSON, matching the frozen flat-object schema) without
// hand-written MarshalJSON per kind. It has to be a distinct identifier from
// "Envelope" because every concrete type also implements the Event interface
// method `Envelope() Envelope` — Go disallows a field and a method of the
// same name on the same type ("field and method with the same name"), so the
// embedded field can't literally be named Envelope. The alias keeps the
// underlying type identical (same struct, same JSON tags) so behavior is
// unaffected; only the field's selector name differs.
type env = Envelope

// Event is implemented by every concrete event kind that flows through
// /ingest and Subscribe(). Blob is data-only (sent via POST /blob, never
// inline on the event stream) and does NOT implement Event.
type Event interface {
	Envelope() Envelope
	Kind() Kind
}

// RequestSummary is the "summary" field of a provider_request event.
type RequestSummary struct {
	SystemPromptHash  string           `json:"systemPromptHash"`
	SystemPromptBytes int              `json:"systemPromptBytes"`
	MessageCount      int              `json:"messageCount"`
	NewMessages       []MessageSummary `json:"newMessages"`
	ToolCount         int              `json:"toolCount"`
	ToolNames         []string         `json:"toolNames"`
	McpToolNames      []string         `json:"mcpToolNames"`
	// ToolSchemaHash is the content hash of the full tool-schema blob (the
	// JSON schemas behind ToolNames/McpToolNames) that the extension posts
	// separately via POST /blob (R2-6). It lets the TUI resolve the schema
	// on demand instead of inlining it on every provider_request event.
	ToolSchemaHash string `json:"toolSchemaHash"`
	EstTokens      int    `json:"estTokens"`
}

// MessageSummary describes one message added since the previous request.
type MessageSummary struct {
	Role    string `json:"role"`
	Bytes   int    `json:"bytes"`
	Hash    string `json:"hash"`
	Preview string `json:"preview"`
}

// UsageSummary is the "usage" field of a provider_response event.
type UsageSummary struct {
	InputTokens  int `json:"inputTokens"`
	OutputTokens int `json:"outputTokens"`
	TotalTokens  int `json:"totalTokens"`
}

// TurnStart marks the start of a turn.
type TurnStart struct {
	env
	Model   string `json:"model"`
	Trigger string `json:"trigger"`
}

func (e TurnStart) Envelope() Envelope { return e.env }
func (e TurnStart) Kind() Kind         { return e.env.Kind }

// ProviderRequest summarizes the plaintext request body sent to the model
// provider for this turn.
type ProviderRequest struct {
	env
	Model        string         `json:"model"`
	Summary      RequestSummary `json:"summary"`
	ChangedBlobs []string       `json:"changedBlobs"`
	// Headers carries the request headers assembled for the provider HTTP
	// call (pi's before_provider_headers hook, which fires AFTER
	// before_provider_request — the extension stashes the rest of this event
	// and attaches Headers once that later hook fires). Mirrors
	// ProviderResponse.Headers below: same json tag, same decode capping
	// (capHeaders). Omitted (nil) when the headers hook never fired for this
	// request (e.g. a stale stash flushed by ordering safety nets).
	Headers map[string]string `json:"headers,omitempty"`
}

func (e ProviderRequest) Envelope() Envelope { return e.env }
func (e ProviderRequest) Kind() Kind         { return e.env.Kind }

// ProviderResponse summarizes the provider's response for this turn.
type ProviderResponse struct {
	env
	Status     int               `json:"status"`
	StopReason string            `json:"stopReason"`
	Usage      *UsageSummary     `json:"usage"`
	Headers    map[string]string `json:"headers,omitempty"`
	// TextBytes/TextPreview/TextHash/ToolCalls capture what the assistant
	// actually GENERATED this turn (R6-1) — previously this event only ever
	// recorded status/usage/headers, so the model's own reply was lost and
	// only reappeared a turn later as a message in the NEXT provider_request.
	// TextHash is the content hash of the full assistant text, which the
	// extension POSTs separately via POST /blob (same first-seen-blob pattern
	// as ToolSchemaHash/ArgsHash/ResultHash) so the TUI can resolve the full
	// reply on demand instead of inlining it on every event.
	TextBytes   int      `json:"textBytes"`
	TextPreview string   `json:"textPreview"`
	TextHash    string   `json:"textHash"`
	ToolCalls   []string `json:"toolCalls"`
}

func (e ProviderResponse) Envelope() Envelope { return e.env }
func (e ProviderResponse) Kind() Kind         { return e.env.Kind }

// ToolStart marks the start of a tool (builtin, skill, or MCP) invocation.
type ToolStart struct {
	env
	ToolID      string `json:"toolId"`
	Source      string `json:"source"`
	Name        string `json:"name"`
	ArgsSummary string `json:"argsSummary"`
	ArgsHash    string `json:"argsHash"`
}

func (e ToolStart) Envelope() Envelope { return e.env }
func (e ToolStart) Kind() Kind         { return e.env.Kind }

// ToolEnd marks the completion of a tool invocation.
type ToolEnd struct {
	env
	ToolID        string `json:"toolId"`
	OK            bool   `json:"ok"`
	ResultBytes   int    `json:"resultBytes"`
	ResultSummary string `json:"resultSummary"`
	ResultHash    string `json:"resultHash"`
	DurationMs    int    `json:"durationMs"`
}

func (e ToolEnd) Envelope() Envelope { return e.env }
func (e ToolEnd) Kind() Kind         { return e.env.Kind }

// ContextEvent covers control-plane signals: skill loads, compaction, model
// changes, thinking-level changes, etc.
type ContextEvent struct {
	env
	CtxKind string `json:"ctxKind"`
	Detail  string `json:"detail"`
}

func (e ContextEvent) Envelope() Envelope { return e.env }
func (e ContextEvent) Kind() Kind         { return e.env.Kind }

// Blob is a content-addressed payload body: system prompt, message text,
// tool args/result, etc. It is data-only — sent via POST /blob, never inline
// on the /ingest or /stream event feed — so it does not implement Event.
type Blob struct {
	Hash  string `json:"hash"`
	Bytes int    `json:"bytes"`
	Text  string `json:"text"`
}

// maxFieldBytes caps individual free-form LONG string fields on decode
// (detail, previews, summaries) — R2-7. hub.go's maxIngestLine already
// bounds the whole line, but a single field this large, retained for as
// long as its event sits in the ring, is still a meaningful chunk of
// memory on its own (2000 ring slots x a field near maxIngestLine adds up
// fast). Truncation happens here, on decode, so it bounds RETAINED memory;
// the wire bytes were already read off the socket regardless.
const maxFieldBytes = 64 << 10 // 64KB

// maxIdBytes caps short identifier/label fields on decode (model, trigger,
// stopReason, source, name, role, ctxKind, turnId/sessionId/sandboxId, and
// individual tool-name entries) — R3-2a. maxFieldBytes alone left every
// OTHER decoded string bounded only by maxIngestLine (1MB): a single
// event's model/name/id field has no business being anywhere near that
// large, so it gets a much tighter, purpose-appropriate cap. 512 bytes is
// generous headroom over any real identifier while still bounding retained
// memory per field to something negligible.
const maxIdBytes = 512

// maxHashBytes caps hash fields defensively. Hashes are fixed-length
// (sha256 hex = 64 chars) by construction, so this should never bind in
// practice — R3-2a is explicit that capping a hash must never happen based
// on content (a truncated preview next to the REAL hash of the full blob is
// fine and expected), only defensively bounds a malformed/oversized value
// arriving under a field name that looks safe.
const maxHashBytes = 128

// maxListEntries caps the LENGTH of decoded string slices (tool-name lists,
// new-message lists, changed-blob-hash lists) — R3-2a. Per-entry field caps
// don't stop an attacker/bug from sending a huge ARRAY of small strings
// instead of one huge string; this bounds that independently.
const maxListEntries = 512

// capField truncates s to maxFieldBytes if it's longer, leaving shorter
// strings (the overwhelming majority) untouched. Truncation is a plain byte
// slice: it can land mid multi-byte UTF-8 rune (producing a possibly-invalid
// trailing rune), which is a deliberate tradeoff — it never panics, and a
// half-rune tail on a preview/detail field that already tells the reader
// it's a truncated preview is cheap to accept versus rune-scanning every
// decoded field.
func capField(s string) string {
	if len(s) <= maxFieldBytes {
		return s
	}
	return s[:maxFieldBytes]
}

// capID truncates s to maxIdBytes if it's longer. Same byte-slice tradeoff
// as capField, just a much tighter bound for fields that are expected to be
// tiny (model names, tool names, session/turn/sandbox ids, ...).
func capID(s string) string {
	if len(s) <= maxIdBytes {
		return s
	}
	return s[:maxIdBytes]
}

// capHash truncates s to maxHashBytes if it's longer. Only a defensive
// backstop (see maxHashBytes) — real hashes never hit this.
func capHash(s string) string {
	if len(s) <= maxHashBytes {
		return s
	}
	return s[:maxHashBytes]
}

// maxHeaderEntries caps the number of Headers entries retained on decode
// (R4-1b), for both ProviderResponse.Headers and ProviderRequest.Headers.
// Per-string caps alone (capID/capField below)
// don't stop an attacker/bug from sending an unbounded NUMBER of small
// headers instead of one huge string; this bounds the map's entry count
// independently, the same way maxListEntries bounds slice length.
const maxHeaderEntries = 64

// capStringSlice caps list to at most maxListEntries entries, then applies
// capFn to every remaining entry in place. Shared by every capped
// string-slice field (tool names, mcp tool names, changed-blob hashes).
func capStringSlice(list []string, capFn func(string) string) []string {
	if len(list) > maxListEntries {
		list = list[:maxListEntries]
	}
	for i := range list {
		list[i] = capFn(list[i])
	}
	return list
}

// capHeaders bounds a decoded Headers map (ProviderResponse.Headers or
// ProviderRequest.Headers) to at most maxHeaderEntries entries (R4-1b) —
// extras are dropped deterministically by
// keeping the lexicographically-first maxHeaderEntries keys (sorted, so
// which entries survive is reproducible instead of depending on Go's
// randomized map iteration order) — and caps every surviving key (capID:
// header names are short identifiers) and value (capField: some real header
// values, e.g. long trace/correlation strings, run longer than a typical
// id) individually. A nil map stays nil.
func capHeaders(h map[string]string) map[string]string {
	if h == nil {
		return nil
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > maxHeaderEntries {
		keys = keys[:maxHeaderEntries]
	}
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		out[capID(k)] = capField(h[k])
	}
	return out
}

// capEnvelopeIDs caps the free-text id fields carried on every event's
// envelope (R3-2a) — sandboxId/sessionId/turnId are attacker-influenced
// (they originate in the sandbox, not the host) and, like every other
// decoded string, were previously bounded only by maxIngestLine.
func capEnvelopeIDs(e *Envelope) {
	e.SandboxID = capID(e.SandboxID)
	e.SessionID = capID(e.SessionID)
	e.TurnID = capID(e.TurnID)
}

// eventSize estimates e's retained memory footprint for Ring's byte budget
// (R2-7): the size of e's own wire encoding. That's the exact number of
// bytes e's largest fields (context_event.detail, tool previews, etc.) would
// occupy if retained in the ring, and it's cheap — one json.Marshal per
// Ring.Add, dwarfed by the HTTP handling already happening around it. Every
// concrete Event type here marshals cleanly (plain structs, no channels or
// funcs), so the error path is essentially unreachable; it falls back to a
// small constant rather than under-counting (and thus under-evicting) if it
// ever does trip.
func eventSize(e Event) int {
	b, err := Encode(e)
	if err != nil {
		return 256
	}
	return len(b)
}

// Decode parses one NDJSON line into its concrete Event type, reading "kind"
// first to pick the target struct. It returns an error for malformed JSON or
// an unrecognized kind (including "blob", which is data-only and never
// appears on the event stream). Every decoded field is capped after
// unmarshal (R2-7, extended by R3-2a): long free-form fields (detail,
// previews, summaries) to maxFieldBytes, short id/label fields (model,
// trigger, stopReason, source, name, role, ctxKind, and every envelope id)
// to maxIdBytes, hash fields defensively to maxHashBytes, and string-slice
// fields (tool-name lists, new-message lists, changed-blob-hash lists) to
// maxListEntries. The line itself is already bounded by hub.go's
// maxIngestLine, but capping every field individually bounds what actually
// stays resident once the event lands in the ring — without this, any
// field NOT explicitly capped was bounded only by the 1MB line limit.
func Decode(line []byte) (Event, error) {
	var probe struct {
		Kind Kind `json:"kind"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("monitor: decode envelope: %w", err)
	}
	switch probe.Kind {
	case KindTurnStart:
		var e TurnStart
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("monitor: decode %s: %w", probe.Kind, err)
		}
		capEnvelopeIDs(&e.env)
		e.Model = capID(e.Model)
		e.Trigger = capID(e.Trigger)
		return e, nil
	case KindProviderRequest:
		var e ProviderRequest
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("monitor: decode %s: %w", probe.Kind, err)
		}
		capEnvelopeIDs(&e.env)
		e.Model = capID(e.Model)
		e.Summary.SystemPromptHash = capHash(e.Summary.SystemPromptHash)
		e.Summary.ToolSchemaHash = capHash(e.Summary.ToolSchemaHash)
		e.Summary.ToolNames = capStringSlice(e.Summary.ToolNames, capID)
		e.Summary.McpToolNames = capStringSlice(e.Summary.McpToolNames, capID)
		e.ChangedBlobs = capStringSlice(e.ChangedBlobs, capHash)
		e.Headers = capHeaders(e.Headers)
		if len(e.Summary.NewMessages) > maxListEntries {
			e.Summary.NewMessages = e.Summary.NewMessages[:maxListEntries]
		}
		for i := range e.Summary.NewMessages {
			e.Summary.NewMessages[i].Role = capID(e.Summary.NewMessages[i].Role)
			e.Summary.NewMessages[i].Hash = capHash(e.Summary.NewMessages[i].Hash)
			e.Summary.NewMessages[i].Preview = capField(e.Summary.NewMessages[i].Preview)
		}
		return e, nil
	case KindProviderResponse:
		var e ProviderResponse
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("monitor: decode %s: %w", probe.Kind, err)
		}
		capEnvelopeIDs(&e.env)
		e.StopReason = capID(e.StopReason)
		e.Headers = capHeaders(e.Headers)
		e.TextPreview = capField(e.TextPreview)
		e.TextHash = capHash(e.TextHash)
		e.ToolCalls = capStringSlice(e.ToolCalls, capID)
		return e, nil
	case KindToolStart:
		var e ToolStart
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("monitor: decode %s: %w", probe.Kind, err)
		}
		capEnvelopeIDs(&e.env)
		e.ToolID = capID(e.ToolID)
		e.Source = capID(e.Source)
		e.Name = capID(e.Name)
		e.ArgsSummary = capField(e.ArgsSummary)
		e.ArgsHash = capHash(e.ArgsHash)
		return e, nil
	case KindToolEnd:
		var e ToolEnd
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("monitor: decode %s: %w", probe.Kind, err)
		}
		capEnvelopeIDs(&e.env)
		e.ToolID = capID(e.ToolID)
		e.ResultSummary = capField(e.ResultSummary)
		e.ResultHash = capHash(e.ResultHash)
		return e, nil
	case KindContextEvent:
		var e ContextEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("monitor: decode %s: %w", probe.Kind, err)
		}
		capEnvelopeIDs(&e.env)
		e.CtxKind = capID(e.CtxKind)
		e.Detail = capField(e.Detail)
		return e, nil
	default:
		return nil, fmt.Errorf("monitor: unknown event kind %q", probe.Kind)
	}
}

// Encode marshals an Event back to its flat NDJSON-line wire form (no
// trailing newline; the caller appends one per architecture.md 2.2).
func Encode(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("monitor: encode %s: %w", e.Kind(), err)
	}
	return b, nil
}
