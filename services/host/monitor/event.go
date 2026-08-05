// Package monitor is the host-side half of `pix monitor`: a debug wiretap
// that records a running sandbox's out-of-sandbox traffic (model
// requests/responses, tool + MCP calls, context/control events) to bounded,
// redacted, file-backed storage and reads it back out.
//
// event.go is the wire contract plus the redaction pass every write goes
// through; store.go the bounded NDJSON store and its filesystem safety
// layer; ingest.go the loopback-only writer and follow.go the reader, which
// share no state, only files.
package monitor

// The wire form is one flat JSON object per line discriminated by "kind",
// field-for-field identical to the in-VM tap (extensions/monitor.ts;
// tests/fixtures/monitor is the shared regression). An unrecognized kind
// decodes to UnknownEvent, not an error, so a newer tap against an older
// host loses nothing. Every decoded string is capped here, which bounds what
// gets RETAINED: ingest bounds a whole line, this bounds each field. Each
// type carries its own cap() and redacted() next to its fields.

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Kind discriminates the flat wire event shape.
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

const (
	// maxFieldBytes caps free-form LONG fields, maxIDBytes short id/label/
	// hash fields, and maxListEntries decoded slice LENGTH — which per-entry
	// caps alone do not (a huge array of tiny strings is the same attack).
	maxFieldBytes  = 64 << 10
	maxIDBytes     = 512
	maxListEntries = 512
)

// Envelope holds the fields present on every event.
type Envelope struct {
	Kind      Kind   `json:"kind"`
	SandboxID string `json:"sandboxId"`
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	Seq       uint64 `json:"seq"`
	TS        int64  `json:"ts"`
}

// Envelope returns e, so embedding one satisfies Event with no per-type
// method; it also flattens the envelope into the wire object.
func (e Envelope) Envelope() Envelope { return e }

// env is Envelope under a second name, because Go forbids a field and a
// method sharing one.
type env = Envelope

// Event is implemented by every concrete kind carried on /ingest.
type Event interface {
	Envelope() Envelope
}

// capEnvelope caps the ids on every event. A free function, not a method, so
// a type that forgets its own cap() cannot silently inherit this one and
// leave its payload uncapped.
func capEnvelope(e *Envelope) { capIDs(&e.SandboxID, &e.SessionID, &e.TurnID) }

// RequestSummary is a provider_request's "summary"; its two hashes name
// bodies the tap POSTs once each to /blob.
type RequestSummary struct {
	SystemPromptHash  string           `json:"systemPromptHash"`
	SystemPromptBytes int              `json:"systemPromptBytes"`
	MessageCount      int              `json:"messageCount"`
	NewMessages       []MessageSummary `json:"newMessages"`
	ToolCount         int              `json:"toolCount"`
	ToolNames         []string         `json:"toolNames"`
	McpToolNames      []string         `json:"mcpToolNames"`
	ToolSchemaHash    string           `json:"toolSchemaHash"`
	EstTokens         int              `json:"estTokens"`
}

// MessageSummary describes one message added since the previous request.
type MessageSummary struct {
	Role    string `json:"role"`
	Bytes   int    `json:"bytes"`
	Hash    string `json:"hash"`
	Preview string `json:"preview"`
}

// UsageSummary is the "usage" field of a provider_response.
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

func (e *TurnStart) cap()           { capEnvelope(&e.env); capIDs(&e.Model, &e.Trigger) }
func (e TurnStart) redacted() Event { e.Model = redactText(e.Model); return e }

// ProviderRequest summarizes the request sent to the provider. Trigger
// ("user" | "tool_result" | "compaction" | "unknown") is duplicated from the
// paired turn_start so a reader never has to cross-reference.
type ProviderRequest struct {
	env
	Model        string         `json:"model"`
	Summary      RequestSummary `json:"summary"`
	ChangedBlobs []string       `json:"changedBlobs"`
	Trigger      string         `json:"trigger"`
}

func (e *ProviderRequest) cap() {
	capEnvelope(&e.env)
	s := &e.Summary
	capIDs(&e.Model, &e.Trigger, &s.SystemPromptHash, &s.ToolSchemaHash)
	s.ToolNames, s.McpToolNames = capIDList(s.ToolNames), capIDList(s.McpToolNames)
	e.ChangedBlobs = capIDList(e.ChangedBlobs)
	s.NewMessages = capLen(s.NewMessages)
	for i := range s.NewMessages {
		m := &s.NewMessages[i]
		capIDs(&m.Role, &m.Hash)
		capFields(&m.Preview)
	}
}
func (e ProviderRequest) redacted() Event {
	e.Model = redactText(e.Model)
	for i := range e.Summary.NewMessages {
		e.Summary.NewMessages[i].Preview = redactText(e.Summary.NewMessages[i].Preview)
	}
	return e
}

// ProviderResponse summarizes the response; TextHash names the full reply.
type ProviderResponse struct {
	env
	Status      int           `json:"status"`
	StopReason  string        `json:"stopReason"`
	Usage       *UsageSummary `json:"usage"`
	TextBytes   int           `json:"textBytes"`
	TextPreview string        `json:"textPreview"`
	TextHash    string        `json:"textHash"`
	ToolCalls   []string      `json:"toolCalls"`
}

func (e *ProviderResponse) cap() {
	capEnvelope(&e.env)
	capIDs(&e.StopReason, &e.TextHash)
	capFields(&e.TextPreview)
	e.ToolCalls = capIDList(e.ToolCalls)
}
func (e ProviderResponse) redacted() Event {
	e.StopReason, e.TextPreview = redactText(e.StopReason), redactText(e.TextPreview)
	return e
}

// ToolStart marks a tool (builtin, skill, or MCP) invocation. InvokesPi is
// computed by the tap from the FULL command text, not from ArgsSummary.
type ToolStart struct {
	env
	ToolID      string `json:"toolId"`
	Source      string `json:"source"`
	Name        string `json:"name"`
	ArgsSummary string `json:"argsSummary"`
	ArgsHash    string `json:"argsHash"`
	InvokesPi   bool   `json:"invokesPi"`
}

func (e *ToolStart) cap() {
	capEnvelope(&e.env)
	capIDs(&e.ToolID, &e.Source, &e.Name, &e.ArgsHash)
	capFields(&e.ArgsSummary)
}
func (e ToolStart) redacted() Event { e.ArgsSummary = redactText(e.ArgsSummary); return e }

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

func (e *ToolEnd) cap() {
	capEnvelope(&e.env)
	capIDs(&e.ToolID, &e.ResultHash)
	capFields(&e.ResultSummary)
}
func (e ToolEnd) redacted() Event { e.ResultSummary = redactText(e.ResultSummary); return e }

// ContextEvent covers control-plane signals: skill loads, compaction, model
// and thinking-level changes.
type ContextEvent struct {
	env
	CtxKind string `json:"ctxKind"`
	Detail  string `json:"detail"`
}

func (e *ContextEvent) cap()           { capEnvelope(&e.env); capIDs(&e.CtxKind); capFields(&e.Detail) }
func (e ContextEvent) redacted() Event { e.Detail = redactText(e.Detail); return e }

// UnknownEvent carries a well-formed event of an unrecognized kind: Raw is
// the whole original line, kept verbatim (once scrubbed) so nothing is lost.
type UnknownEvent struct {
	env
	Raw []byte
}

// MarshalJSON returns Raw verbatim; the empty case is a hand-built zero
// value, since decodeUnknown always sets Raw.
func (e UnknownEvent) MarshalJSON() ([]byte, error) {
	if len(e.Raw) == 0 {
		return json.Marshal(e.env)
	}
	return e.Raw, nil
}

// redacted has no known field shape, so it scrubs the WHOLE raw line. Safe
// because every pattern matches a token-like charset with no '"', so a match
// inside a JSON string is replaced without corrupting the structure.
func (e UnknownEvent) redacted() Event { e.Raw = []byte(redactText(string(e.Raw))); return e }

// capIDs and capFields truncate each target in place to its bound. Slicing
// bytes can land mid-rune, accepted deliberately: it never panics, and a
// half-rune tail on a preview beats rune-scanning every field.
func capIDs(ss ...*string)    { capEach(maxIDBytes, ss) }
func capFields(ss ...*string) { capEach(maxFieldBytes, ss) }
func capEach(limit int, ss []*string) {
	for _, p := range ss {
		if len(*p) > limit {
			*p = (*p)[:limit]
		}
	}
}

// capLen caps a decoded slice's length; capIDList also caps each entry.
func capLen[T any](list []T) []T {
	if len(list) > maxListEntries {
		return list[:maxListEntries]
	}
	return list
}
func capIDList(list []string) []string {
	list = capLen(list)
	for i := range list {
		capIDs(&list[i])
	}
	return list
}

// capper is the pointer form of a decodable event: it caps itself in place.
type capper[T any] interface {
	*T
	cap()
}

// Decode parses one NDJSON line into its concrete Event and caps every
// decoded string. It errors on malformed JSON, a missing kind, or "blob"
// (data-only: POSTed to /blob, never on the event stream); an unrecognized
// kind is forward compatibility, not an error (see decodeUnknown).
func Decode(line []byte) (Event, error) {
	var probe struct {
		Kind Kind `json:"kind"`
	}
	switch err := json.Unmarshal(line, &probe); {
	case err != nil:
		return nil, fmt.Errorf("monitor: decode envelope: %w", err)
	case probe.Kind == "":
		return nil, fmt.Errorf("monitor: missing event kind")
	case probe.Kind == KindBlob:
		return nil, fmt.Errorf("monitor: %q is data-only and never appears on the event stream", probe.Kind)
	}
	switch probe.Kind {
	case KindTurnStart:
		return decodeAs[TurnStart, *TurnStart](line, probe.Kind)
	case KindProviderRequest:
		return decodeAs[ProviderRequest, *ProviderRequest](line, probe.Kind)
	case KindProviderResponse:
		return decodeAs[ProviderResponse, *ProviderResponse](line, probe.Kind)
	case KindToolStart:
		return decodeAs[ToolStart, *ToolStart](line, probe.Kind)
	case KindToolEnd:
		return decodeAs[ToolEnd, *ToolEnd](line, probe.Kind)
	case KindContextEvent:
		return decodeAs[ContextEvent, *ContextEvent](line, probe.Kind)
	}
	return decodeUnknown(line, probe.Kind)
}

// decodeAs unmarshals line into T and applies T's own capping pass.
func decodeAs[T Event, P capper[T]](line []byte, kind Kind) (Event, error) {
	var e T
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, fmt.Errorf("monitor: decode %s: %w", kind, err)
	}
	P(&e).cap()
	return e, nil
}

// decodeUnknown builds an UnknownEvent, decoding the envelope best-effort
// and bounding Raw like any other retained field.
func decodeUnknown(line []byte, kind Kind) (Event, error) {
	var e Envelope
	_ = json.Unmarshal(line, &e)
	e.Kind = kind
	capEnvelope(&e)
	// A whole unknown line has no field shape to cap individually, so it is
	// bounded as one blob.
	raw := append([]byte(nil), line...)
	if maxRaw := maxFieldBytes * 4; len(raw) > maxRaw {
		raw = raw[:maxRaw]
	}
	return UnknownEvent{env: e, Raw: raw}, nil
}

// Encode marshals an Event to its flat wire line, newline excluded.
func Encode(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("monitor: encode %s: %w", e.Envelope().Kind, err)
	}
	return b, nil
}

// ─── redaction ──────────────────────────────────────────────────────────────
//
// The host-side line of defense against a secret landing on disk in
// cleartext. The tap sends hashes and short previews, but a tool's
// argsSummary/resultSummary — or a blob body, the one place full text
// crosses the wire — can carry a real secret. Everything this package writes
// is written redacted; nothing is redacted after the fact. Pattern matching,
// not a security boundary: recognizable secret SHAPES, deliberately
// over-inclusive, not arbitrary sensitive text.

// redactionMarker replaces a matched span. Free of characters ('"', '\\')
// that could corrupt the JSON it sits inside.
const redactionMarker = "[REDACTED]"

// secretPatterns are recognizable secret shapes. Each is applied
// independently so overlapping matches from different patterns all land.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),             // AWS access key id
	regexp.MustCompile(`gh[oprsu]_[A-Za-z0-9]{20,}`),   // GitHub tokens
	regexp.MustCompile(`xox[baprs]-[0-9A-Za-z-]{10,}`), // Slack tokens
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),        // OpenAI/Anthropic keys
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`),
	// Catch-all `key = value` / `"token": "value"` assignment shape: a
	// secret-shaped NAME, an assignment operator, then a long opaque VALUE.
	regexp.MustCompile(`(?i)(api[_-]?key|secret|token|password|passwd)["']?\s*[:=]\s*["']?[A-Za-z0-9/_.+-]{12,}`),
}

// redactText replaces every secret-shaped substring with redactionMarker.
func redactText(s string) string {
	if s == "" {
		return s
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redactionMarker)
	}
	return s
}

// redact returns a scrubbed copy of e from the type's own redacted() (value
// receiver, so the caller's event is not replaced). Short label fields are
// scrubbed too — cheap, and defends against a hostile tap hiding a secret in
// one — but hashes are not, being content-addresses.
func redact(e Event) Event {
	if r, ok := e.(interface{ redacted() Event }); ok {
		return r.redacted()
	}
	return e
}
