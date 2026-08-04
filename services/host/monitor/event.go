package monitor

// event.go is the WIRE CONTRACT with the in-VM tap (extensions/monitor.ts):
// one flat JSON object per line, discriminated by "kind", field-for-field
// identical to that emitter (tests/fixtures/monitor is the shared
// regression). An unrecognized kind decodes to UnknownEvent, not an error,
// so a newer tap against an older host loses nothing. Every decoded string
// is capped here, which is what bounds what gets RETAINED: the ingest server
// bounds a whole line, this bounds each field.

import (
	"encoding/json"
	"fmt"
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

// Envelope returns e, so every event that EMBEDS an envelope satisfies Event
// with no per-type method; embedding also flattens it into the wire object.
func (e Envelope) Envelope() Envelope { return e }

// env is Envelope under a second name, because Go forbids a field and a
// method sharing one.
type env = Envelope

// Event is implemented by every concrete kind carried on /ingest. Kind is
// read off the envelope, not a second interface method.
type Event interface {
	Envelope() Envelope
}

// RequestSummary is a provider_request's "summary". SystemPromptHash and
// ToolSchemaHash name bodies the tap POSTs once each to /blob.
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

// ProviderResponse summarizes the response; TextHash names the full reply,
// POSTed once to /blob.
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

// ContextEvent covers control-plane signals: skill loads, compaction, model
// and thinking-level changes.
type ContextEvent struct {
	env
	CtxKind string `json:"ctxKind"`
	Detail  string `json:"detail"`
}

// UnknownEvent carries a well-formed event of an unrecognized kind. Raw is
// the whole original line, returned verbatim by MarshalJSON once redaction
// has scrubbed it, so nothing is lost.
type UnknownEvent struct {
	env
	Raw []byte
}

// MarshalJSON returns e.Raw verbatim, or just the envelope for a hand-built
// zero value (decodeUnknown always sets Raw).
func (e UnknownEvent) MarshalJSON() ([]byte, error) {
	if len(e.Raw) == 0 {
		return json.Marshal(e.env)
	}
	return e.Raw, nil
}

// capField and capID truncate to their bounds. The plain byte slice can land
// mid-rune, accepted deliberately: it never panics, and a half-rune tail on
// a field already labelled a preview beats rune-scanning every field.
func capField(s string) string {
	if len(s) <= maxFieldBytes {
		return s
	}
	return s[:maxFieldBytes]
}

func capID(s string) string {
	if len(s) <= maxIDBytes {
		return s
	}
	return s[:maxIDBytes]
}

// capIDs caps list to maxListEntries entries of maxIDBytes each.
func capIDs(list []string) []string {
	if len(list) > maxListEntries {
		list = list[:maxListEntries]
	}
	for i := range list {
		list[i] = capID(list[i])
	}
	return list
}

// capEnvelope caps the wire-supplied ids carried on every event.
func capEnvelope(e *Envelope) {
	e.SandboxID, e.SessionID, e.TurnID = capID(e.SandboxID), capID(e.SessionID), capID(e.TurnID)
}

// Decode parses one NDJSON line into its concrete Event, reading "kind"
// first to pick the struct, and caps every decoded string. It errors on
// malformed JSON, a missing kind, or "blob" (data-only: POSTed to /blob,
// never on the event stream).
func Decode(line []byte) (Event, error) {
	var probe struct {
		Kind Kind `json:"kind"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return nil, fmt.Errorf("monitor: decode envelope: %w", err)
	}
	switch probe.Kind {
	case "":
		return nil, fmt.Errorf("monitor: missing event kind")
	case KindBlob:
		return nil, fmt.Errorf("monitor: %q is data-only and never appears on the event stream", probe.Kind)
	case KindTurnStart:
		return decodeAs(line, probe.Kind, func(e *TurnStart) {
			capEnvelope(&e.env)
			e.Model, e.Trigger = capID(e.Model), capID(e.Trigger)
		})
	case KindProviderRequest:
		return decodeAs(line, probe.Kind, func(e *ProviderRequest) {
			capEnvelope(&e.env)
			e.Model, e.Trigger = capID(e.Model), capID(e.Trigger)
			e.Summary.SystemPromptHash = capID(e.Summary.SystemPromptHash)
			e.Summary.ToolSchemaHash = capID(e.Summary.ToolSchemaHash)
			e.Summary.ToolNames = capIDs(e.Summary.ToolNames)
			e.Summary.McpToolNames = capIDs(e.Summary.McpToolNames)
			e.ChangedBlobs = capIDs(e.ChangedBlobs)
			if len(e.Summary.NewMessages) > maxListEntries {
				e.Summary.NewMessages = e.Summary.NewMessages[:maxListEntries]
			}
			for i := range e.Summary.NewMessages {
				m := &e.Summary.NewMessages[i]
				m.Role, m.Hash, m.Preview = capID(m.Role), capID(m.Hash), capField(m.Preview)
			}
		})
	case KindProviderResponse:
		return decodeAs(line, probe.Kind, func(e *ProviderResponse) {
			capEnvelope(&e.env)
			e.StopReason, e.TextHash = capID(e.StopReason), capID(e.TextHash)
			e.TextPreview = capField(e.TextPreview)
			e.ToolCalls = capIDs(e.ToolCalls)
		})
	case KindToolStart:
		return decodeAs(line, probe.Kind, func(e *ToolStart) {
			capEnvelope(&e.env)
			e.ToolID, e.Source, e.Name = capID(e.ToolID), capID(e.Source), capID(e.Name)
			e.ArgsSummary, e.ArgsHash = capField(e.ArgsSummary), capID(e.ArgsHash)
		})
	case KindToolEnd:
		return decodeAs(line, probe.Kind, func(e *ToolEnd) {
			capEnvelope(&e.env)
			e.ToolID, e.ResultHash = capID(e.ToolID), capID(e.ResultHash)
			e.ResultSummary = capField(e.ResultSummary)
		})
	case KindContextEvent:
		return decodeAs(line, probe.Kind, func(e *ContextEvent) {
			capEnvelope(&e.env)
			e.CtxKind, e.Detail = capID(e.CtxKind), capField(e.Detail)
		})
	default:
		return decodeUnknown(line, probe.Kind)
	}
}

// decodeAs unmarshals line into T and applies T's own capping pass.
func decodeAs[T Event](line []byte, kind Kind, fix func(*T)) (Event, error) {
	var e T
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, fmt.Errorf("monitor: decode %s: %w", kind, err)
	}
	fix(&e)
	return e, nil
}

// decodeUnknown builds an UnknownEvent, decoding the envelope best-effort
// (an unrecognized kind may carry an unrecognized envelope) and bounding Raw
// like any other retained field.
func decodeUnknown(line []byte, kind Kind) (Event, error) {
	var e Envelope
	_ = json.Unmarshal(line, &e)
	e.Kind = kind
	capEnvelope(&e)
	raw := append([]byte(nil), line...)
	if len(raw) > maxFieldBytes*4 {
		raw = raw[:maxFieldBytes*4]
	}
	return UnknownEvent{env: e, Raw: raw}, nil
}

// Encode marshals an Event to its flat NDJSON-line form, without the newline
// the caller appends.
func Encode(e Event) ([]byte, error) {
	b, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("monitor: encode %s: %w", e.Envelope().Kind, err)
	}
	return b, nil
}
