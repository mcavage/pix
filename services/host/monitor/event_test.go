package monitor

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func sampleEnvelope(kind Kind) env {
	return env{
		Kind:      kind,
		SandboxID: "sbx-1",
		SessionID: "sess-1",
		TurnID:    "turn-1",
		Seq:       7,
		TS:        1700000000000,
	}
}

// TestEventEncodeDecodeRoundTrip covers every concrete kind: Encode then
// Decode must reproduce an equal value, and Envelope()/Kind() must agree with
// the envelope embedded at construction.
func TestEventEncodeDecodeRoundTrip(t *testing.T) {
	cases := []Event{
		TurnStart{
			env:     sampleEnvelope(KindTurnStart),
			Model:   "claude-opus-4-8",
			Trigger: "user",
		},
		ProviderRequest{
			env:   sampleEnvelope(KindProviderRequest),
			Model: "claude-opus-4-8",
			Summary: RequestSummary{
				SystemPromptHash:  "hash-sys",
				SystemPromptBytes: 41000,
				MessageCount:      12,
				NewMessages: []MessageSummary{
					{Role: "user", Bytes: 42, Hash: "hash-m1", Preview: "hello"},
				},
				ToolCount:      14,
				ToolNames:      []string{"bash", "read"},
				McpToolNames:   []string{"slack_post"},
				ToolSchemaHash: "hash-schema",
				EstTokens:      38000,
			},
			ChangedBlobs: []string{"hash-sys"},
		},
		ProviderResponse{
			env:        sampleEnvelope(KindProviderResponse),
			Status:     200,
			StopReason: "tool_use",
			Usage:      &UsageSummary{InputTokens: 37900, OutputTokens: 512, TotalTokens: 38412},
			Headers:    map[string]string{"x-request-id": "abc"},
		},
		ProviderResponse{
			env:        sampleEnvelope(KindProviderResponse),
			Status:     200,
			StopReason: "",
			Usage:      nil,
		},
		ToolStart{
			env:         sampleEnvelope(KindToolStart),
			ToolID:      "t1",
			Source:      "builtin",
			Name:        "bash",
			ArgsSummary: "go test ./...",
			ArgsHash:    "hash-args",
		},
		ToolEnd{
			env:           sampleEnvelope(KindToolEnd),
			ToolID:        "t1",
			OK:            true,
			ResultBytes:   2100,
			ResultSummary: "ok",
			ResultHash:    "hash-result",
			DurationMs:    4300,
		},
		ContextEvent{
			env:     sampleEnvelope(KindContextEvent),
			CtxKind: "model_change",
			Detail:  "switched to opus",
		},
	}

	for _, want := range cases {
		t.Run(string(want.Kind()), func(t *testing.T) {
			line, err := Encode(want)
			if err != nil {
				t.Fatalf("Encode() error: %v", err)
			}
			got, err := Decode(line)
			if err != nil {
				t.Fatalf("Decode() error: %v", err)
			}
			gotLine, err := Encode(got)
			if err != nil {
				t.Fatalf("re-Encode() error: %v", err)
			}
			if string(gotLine) != string(line) {
				t.Fatalf("round-trip mismatch:\n  want %s\n  got  %s", line, gotLine)
			}
			if got.Envelope() != want.Envelope() {
				t.Fatalf("Envelope() = %+v, want %+v", got.Envelope(), want.Envelope())
			}
			if got.Kind() != want.Kind() {
				t.Fatalf("Kind() = %q, want %q", got.Kind(), want.Kind())
			}
		})
	}
}

func TestDecodeUnknownKindErrors(t *testing.T) {
	_, err := Decode([]byte(`{"kind":"nope","sandboxId":"x"}`))
	if err == nil {
		t.Fatalf("Decode() with unknown kind: got nil error, want an error")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("Decode() error = %q, want it to mention the unknown kind", err.Error())
	}
}

func TestDecodeBlobKindErrors(t *testing.T) {
	// blob is data-only (POST /blob), never a decodable Event on the stream.
	_, err := Decode([]byte(`{"kind":"blob"}`))
	if err == nil {
		t.Fatalf("Decode() with kind=blob: got nil error, want an error (blob is not an Event)")
	}
}

func TestDecodeMalformedJSONErrors(t *testing.T) {
	_, err := Decode([]byte(`not json`))
	if err == nil {
		t.Fatalf("Decode() with malformed JSON: got nil error, want an error")
	}
}

// TestEventJSONTagsGolden pins the exact wire field names (architecture.md
// Section 2.3) so Unit D's TypeScript emitter can be checked against the same
// shape by eye. Any change here is a wire-protocol change.
func TestEventJSONTagsGolden(t *testing.T) {
	ts := TurnStart{
		env:     env{Kind: KindTurnStart, SandboxID: "sbx", SessionID: "sess", TurnID: "t1", Seq: 3, TS: 1234},
		Model:   "opus",
		Trigger: "user",
	}
	line, err := Encode(ts)
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	want := `{"kind":"turn_start","sandboxId":"sbx","sessionId":"sess","turnId":"t1","seq":3,"ts":1234,"model":"opus","trigger":"user"}`
	if string(line) != want {
		t.Fatalf("turn_start JSON =\n  %s\nwant\n  %s", line, want)
	}

	pr := ProviderRequest{
		env:   env{Kind: KindProviderRequest, Seq: 1},
		Model: "opus",
		Summary: RequestSummary{
			SystemPromptHash:  "h",
			SystemPromptBytes: 1,
			MessageCount:      1,
			NewMessages:       []MessageSummary{{Role: "user", Bytes: 1, Hash: "h2", Preview: "p"}},
			ToolCount:         1,
			ToolNames:         []string{"bash"},
			McpToolNames:      []string{"slack_post"},
			ToolSchemaHash:    "hash-schema",
			EstTokens:         10,
		},
		ChangedBlobs: []string{"h"},
	}
	var m map[string]json.RawMessage
	line, err = Encode(pr)
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("Unmarshal() error: %v", err)
	}
	for _, key := range []string{"kind", "sandboxId", "sessionId", "turnId", "seq", "ts", "model", "summary", "changedBlobs"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("provider_request JSON missing key %q; got keys %v", key, m)
		}
	}
	var summary map[string]json.RawMessage
	if err := json.Unmarshal(m["summary"], &summary); err != nil {
		t.Fatalf("Unmarshal(summary) error: %v", err)
	}
	for _, key := range []string{"systemPromptHash", "systemPromptBytes", "messageCount", "newMessages", "toolCount", "toolNames", "mcpToolNames", "toolSchemaHash", "estTokens"} {
		if _, ok := summary[key]; !ok {
			t.Fatalf("summary JSON missing key %q; got keys %v", key, summary)
		}
	}

	ctx := ContextEvent{env: env{Kind: KindContextEvent}, CtxKind: "compaction", Detail: "d"}
	line, err = Encode(ctx)
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	if !strings.Contains(string(line), `"ctxKind":"compaction"`) {
		t.Fatalf("context_event JSON = %s, want ctxKind key", line)
	}

	b, err := json.Marshal(Blob{Hash: "h", Bytes: 3, Text: "abc"})
	if err != nil {
		t.Fatalf("Marshal(Blob) error: %v", err)
	}
	wantBlob := `{"hash":"h","bytes":3,"text":"abc"}`
	if string(b) != wantBlob {
		t.Fatalf("Blob JSON = %s, want %s", b, wantBlob)
	}
}

// TestDecodeCapsOversizedFreeTextFields is R3-2a: EVERY decoded string and
// string slice must be bounded by a purpose-appropriate cap, not just the
// four fields (Detail/ArgsSummary/ResultSummary/Preview) capped for R2-7.
// This exercises an oversized envelope id, a top-level free-text field, a
// hash field, and an oversized ToolNames array all on one event.
func TestDecodeCapsOversizedFreeTextFields(t *testing.T) {
	hugeModel := strings.Repeat("m", 10_000)
	hugeSandboxID := strings.Repeat("s", 10_000)
	hugeHash := strings.Repeat("a", 10_000)
	toolNames := make([]string, 1000)
	for i := range toolNames {
		toolNames[i] = fmt.Sprintf("tool-%d-%s", i, strings.Repeat("z", 2000))
	}

	line, err := Encode(ProviderRequest{
		env:   env{Kind: KindProviderRequest, SandboxID: hugeSandboxID},
		Model: hugeModel,
		Summary: RequestSummary{
			SystemPromptHash: hugeHash,
			ToolNames:        toolNames,
		},
	})
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}

	got, err := Decode(line)
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	pr, ok := got.(ProviderRequest)
	if !ok {
		t.Fatalf("Decode() returned %T, want ProviderRequest", got)
	}

	if got := len(pr.Model); got > maxIdBytes {
		t.Fatalf("Model len = %d, want <= maxIdBytes (%d)", got, maxIdBytes)
	}
	if got := len(pr.Envelope().SandboxID); got > maxIdBytes {
		t.Fatalf("SandboxID len = %d, want <= maxIdBytes (%d)", got, maxIdBytes)
	}
	if got := len(pr.Summary.SystemPromptHash); got > maxHashBytes {
		t.Fatalf("SystemPromptHash len = %d, want <= maxHashBytes (%d)", got, maxHashBytes)
	}
	if got := len(pr.Summary.ToolNames); got > maxListEntries {
		t.Fatalf("ToolNames len = %d, want <= maxListEntries (%d)", got, maxListEntries)
	}
	for i, name := range pr.Summary.ToolNames {
		if got := len(name); got > maxIdBytes {
			t.Fatalf("ToolNames[%d] len = %d, want <= maxIdBytes (%d)", i, got, maxIdBytes)
		}
	}
}

// TestDecodeCapsOversizedFieldMultibyteUTF8 confirms capping an oversized
// free-text field that lands mid multi-byte UTF-8 rune does not panic —
// capField/capID truncate by BYTE length (not rune-aware), a deliberate,
// documented tradeoff; it must never panic even when the cap boundary
// splits a multi-byte character.
func TestDecodeCapsOversizedFieldMultibyteUTF8(t *testing.T) {
	// "世" is 3 bytes in UTF-8. Repeating it maxIdBytes times yields a string
	// whose byte length (3*maxIdBytes) is not a multiple of 1 in a way that
	// lets the maxIdBytes-th byte land inside a rune, forcing the split case.
	hugeName := strings.Repeat("世", maxIdBytes)
	hugeDetail := strings.Repeat("界", maxFieldBytes)

	line, err := Encode(ToolStart{
		env:  env{Kind: KindToolStart},
		Name: hugeName,
	})
	if err != nil {
		t.Fatalf("Encode(ToolStart) error: %v", err)
	}
	got, err := Decode(line)
	if err != nil {
		t.Fatalf("Decode(ToolStart) error: %v", err)
	}
	ts := got.(ToolStart)
	if n := len(ts.Name); n != maxIdBytes {
		t.Fatalf("Name len = %d, want exactly %d", n, maxIdBytes)
	}

	line, err = Encode(ContextEvent{
		env:    env{Kind: KindContextEvent},
		Detail: hugeDetail,
	})
	if err != nil {
		t.Fatalf("Encode(ContextEvent) error: %v", err)
	}
	got, err = Decode(line)
	if err != nil {
		t.Fatalf("Decode(ContextEvent) error: %v", err)
	}
	ce := got.(ContextEvent)
	if n := len(ce.Detail); n != maxFieldBytes {
		t.Fatalf("Detail len = %d, want exactly %d", n, maxFieldBytes)
	}
}

// TestDecodeCapsToolIDAndHeaders is R4-1: two spots the round-3 decode
// capping missed. ToolStart/ToolEnd.ToolID was never capped (an oversized
// toolId passes the ingest-line limit and lands in TUI row keys), and
// ProviderResponse.Headers retained an unbounded number of uncapped
// key/value strings.
func TestDecodeCapsToolIDAndHeaders(t *testing.T) {
	hugeID := strings.Repeat("t", 10_000)

	line, err := Encode(ToolStart{env: env{Kind: KindToolStart}, ToolID: hugeID, Name: "bash"})
	if err != nil {
		t.Fatalf("Encode(ToolStart) error: %v", err)
	}
	got, err := Decode(line)
	if err != nil {
		t.Fatalf("Decode(ToolStart) error: %v", err)
	}
	ts := got.(ToolStart)
	if n := len(ts.ToolID); n > maxIdBytes {
		t.Fatalf("ToolStart.ToolID len = %d, want <= maxIdBytes (%d)", n, maxIdBytes)
	}

	line, err = Encode(ToolEnd{env: env{Kind: KindToolEnd}, ToolID: hugeID})
	if err != nil {
		t.Fatalf("Encode(ToolEnd) error: %v", err)
	}
	got, err = Decode(line)
	if err != nil {
		t.Fatalf("Decode(ToolEnd) error: %v", err)
	}
	te := got.(ToolEnd)
	if n := len(te.ToolID); n > maxIdBytes {
		t.Fatalf("ToolEnd.ToolID len = %d, want <= maxIdBytes (%d)", n, maxIdBytes)
	}

	// Headers: more than maxHeaderEntries entries, each with an oversized key
	// and value, must come out bounded on both count and per-string size.
	headers := make(map[string]string, maxHeaderEntries+50)
	for i := 0; i < maxHeaderEntries+50; i++ {
		key := fmt.Sprintf("x-header-%03d-%s", i, strings.Repeat("k", 2000))
		headers[key] = strings.Repeat("v", 2000)
	}
	line, err = Encode(ProviderResponse{env: env{Kind: KindProviderResponse}, Headers: headers})
	if err != nil {
		t.Fatalf("Encode(ProviderResponse) error: %v", err)
	}
	got, err = Decode(line)
	if err != nil {
		t.Fatalf("Decode(ProviderResponse) error: %v", err)
	}
	pr := got.(ProviderResponse)
	if n := len(pr.Headers); n > maxHeaderEntries {
		t.Fatalf("Headers len = %d, want <= maxHeaderEntries (%d)", n, maxHeaderEntries)
	}
	for k, v := range pr.Headers {
		if n := len(k); n > maxIdBytes {
			t.Fatalf("Headers key len = %d, want <= maxIdBytes (%d): %q", n, maxIdBytes, k)
		}
		if n := len(v); n > maxFieldBytes {
			t.Fatalf("Headers value len = %d, want <= maxFieldBytes (%d)", n, maxFieldBytes)
		}
	}
}

// TestDecodeCapsEveryStringBearingField is table-driven coverage (R4-1,
// closed out further by R5-1) asserting that Decode caps every
// string-bearing field across every event kind, given a maximally oversized
// input for that field. Each case builds one event with a single field blown
// out far past its cap, decodes it, and checks the resulting field never
// exceeds its documented cap. This includes the NESTED ProviderRequest.Summary
// scalar fields (SystemPromptHash, ToolSchemaHash, and one NewMessages
// entry's Role/Hash/Preview) that R4-1's table missed (R5-1). The
// length-bounded SLICE fields nested under Summary (ToolNames, McpToolNames,
// NewMessages) and ChangedBlobs get their own companion tables below —
// TestDecodeCapsProviderRequestStringSlices and
// TestDecodeCapsProviderRequestNewMessages — since capping a slice needs
// both an oversized entry AND an over-length slice, which doesn't fit this
// table's one-field-per-case shape.
func TestDecodeCapsEveryStringBearingField(t *testing.T) {
	hugeID := strings.Repeat("i", 10_000)
	hugeField := strings.Repeat("f", 10_000)
	hugeHash := strings.Repeat("h", 10_000)

	cases := []struct {
		name string
		line func() ([]byte, error)
		max  int
		get  func(Event) string
	}{
		{"TurnStart.Model", func() ([]byte, error) { return Encode(TurnStart{env: env{Kind: KindTurnStart}, Model: hugeID}) }, maxIdBytes, func(e Event) string { return e.(TurnStart).Model }},
		{"TurnStart.Trigger", func() ([]byte, error) { return Encode(TurnStart{env: env{Kind: KindTurnStart}, Trigger: hugeID}) }, maxIdBytes, func(e Event) string { return e.(TurnStart).Trigger }},
		{"ProviderRequest.Model", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Model: hugeID})
		}, maxIdBytes, func(e Event) string { return e.(ProviderRequest).Model }},
		{"ProviderResponse.StopReason", func() ([]byte, error) {
			return Encode(ProviderResponse{env: env{Kind: KindProviderResponse}, StopReason: hugeID})
		}, maxIdBytes, func(e Event) string { return e.(ProviderResponse).StopReason }},
		{"ToolStart.ToolID", func() ([]byte, error) { return Encode(ToolStart{env: env{Kind: KindToolStart}, ToolID: hugeID}) }, maxIdBytes, func(e Event) string { return e.(ToolStart).ToolID }},
		{"ToolStart.Source", func() ([]byte, error) { return Encode(ToolStart{env: env{Kind: KindToolStart}, Source: hugeID}) }, maxIdBytes, func(e Event) string { return e.(ToolStart).Source }},
		{"ToolStart.Name", func() ([]byte, error) { return Encode(ToolStart{env: env{Kind: KindToolStart}, Name: hugeID}) }, maxIdBytes, func(e Event) string { return e.(ToolStart).Name }},
		{"ToolStart.ArgsSummary", func() ([]byte, error) {
			return Encode(ToolStart{env: env{Kind: KindToolStart}, ArgsSummary: hugeField})
		}, maxFieldBytes, func(e Event) string { return e.(ToolStart).ArgsSummary }},
		{"ToolStart.ArgsHash", func() ([]byte, error) { return Encode(ToolStart{env: env{Kind: KindToolStart}, ArgsHash: hugeHash}) }, maxHashBytes, func(e Event) string { return e.(ToolStart).ArgsHash }},
		{"ToolEnd.ToolID", func() ([]byte, error) { return Encode(ToolEnd{env: env{Kind: KindToolEnd}, ToolID: hugeID}) }, maxIdBytes, func(e Event) string { return e.(ToolEnd).ToolID }},
		{"ToolEnd.ResultSummary", func() ([]byte, error) { return Encode(ToolEnd{env: env{Kind: KindToolEnd}, ResultSummary: hugeField}) }, maxFieldBytes, func(e Event) string { return e.(ToolEnd).ResultSummary }},
		{"ToolEnd.ResultHash", func() ([]byte, error) { return Encode(ToolEnd{env: env{Kind: KindToolEnd}, ResultHash: hugeHash}) }, maxHashBytes, func(e Event) string { return e.(ToolEnd).ResultHash }},
		{"ContextEvent.CtxKind", func() ([]byte, error) { return Encode(ContextEvent{env: env{Kind: KindContextEvent}, CtxKind: hugeID}) }, maxIdBytes, func(e Event) string { return e.(ContextEvent).CtxKind }},
		{"ContextEvent.Detail", func() ([]byte, error) {
			return Encode(ContextEvent{env: env{Kind: KindContextEvent}, Detail: hugeField})
		}, maxFieldBytes, func(e Event) string { return e.(ContextEvent).Detail }},
		{"Envelope.SandboxID", func() ([]byte, error) { return Encode(TurnStart{env: env{Kind: KindTurnStart, SandboxID: hugeID}}) }, maxIdBytes, func(e Event) string { return e.Envelope().SandboxID }},
		{"Envelope.SessionID", func() ([]byte, error) { return Encode(TurnStart{env: env{Kind: KindTurnStart, SessionID: hugeID}}) }, maxIdBytes, func(e Event) string { return e.Envelope().SessionID }},
		{"Envelope.TurnID", func() ([]byte, error) { return Encode(TurnStart{env: env{Kind: KindTurnStart, TurnID: hugeID}}) }, maxIdBytes, func(e Event) string { return e.Envelope().TurnID }},
		{"ProviderRequest.Summary.SystemPromptHash", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Summary: RequestSummary{SystemPromptHash: hugeHash}})
		}, maxHashBytes, func(e Event) string { return e.(ProviderRequest).Summary.SystemPromptHash }},
		{"ProviderRequest.Summary.ToolSchemaHash", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Summary: RequestSummary{ToolSchemaHash: hugeHash}})
		}, maxHashBytes, func(e Event) string { return e.(ProviderRequest).Summary.ToolSchemaHash }},
		{"ProviderRequest.Summary.NewMessages[0].Role", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Summary: RequestSummary{NewMessages: []MessageSummary{{Role: hugeID}}}})
		}, maxIdBytes, func(e Event) string { return e.(ProviderRequest).Summary.NewMessages[0].Role }},
		{"ProviderRequest.Summary.NewMessages[0].Hash", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Summary: RequestSummary{NewMessages: []MessageSummary{{Hash: hugeHash}}}})
		}, maxHashBytes, func(e Event) string { return e.(ProviderRequest).Summary.NewMessages[0].Hash }},
		{"ProviderRequest.Summary.NewMessages[0].Preview", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Summary: RequestSummary{NewMessages: []MessageSummary{{Preview: hugeField}}}})
		}, maxFieldBytes, func(e Event) string { return e.(ProviderRequest).Summary.NewMessages[0].Preview }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := tc.line()
			if err != nil {
				t.Fatalf("Encode() error: %v", err)
			}
			got, err := Decode(line)
			if err != nil {
				t.Fatalf("Decode() error: %v", err)
			}
			if n := len(tc.get(got)); n > tc.max {
				t.Fatalf("%s len = %d, want <= %d", tc.name, n, tc.max)
			}
		})
	}
}

// TestDecodeCapsProviderRequestStringSlices is R5-1 companion coverage for
// the three plain string slices Decode length-caps to maxListEntries and
// per-entry-caps: ProviderRequest.Summary.ToolNames (capID),
// Summary.McpToolNames (capID), and ProviderRequest.ChangedBlobs (capHash).
// Each case builds a slice with more than maxListEntries entries, every
// entry ALSO individually oversized past its per-entry cap, so a single
// decode exercises both caps at once (an over-length slice of otherwise-tiny
// strings would never catch a missing per-entry capFn call, and vice versa).
func TestDecodeCapsProviderRequestStringSlices(t *testing.T) {
	overLen := maxListEntries + 50
	hugeID := strings.Repeat("i", maxIdBytes+1)
	hugeHash := strings.Repeat("h", maxHashBytes+1)

	oversizedSlice := func(n int, val string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = val
		}
		return out
	}

	cases := []struct {
		name     string
		line     func() ([]byte, error)
		entryMax int
		get      func(ProviderRequest) []string
	}{
		{"Summary.ToolNames", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Summary: RequestSummary{ToolNames: oversizedSlice(overLen, hugeID)}})
		}, maxIdBytes, func(pr ProviderRequest) []string { return pr.Summary.ToolNames }},
		{"Summary.McpToolNames", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, Summary: RequestSummary{McpToolNames: oversizedSlice(overLen, hugeID)}})
		}, maxIdBytes, func(pr ProviderRequest) []string { return pr.Summary.McpToolNames }},
		{"ChangedBlobs", func() ([]byte, error) {
			return Encode(ProviderRequest{env: env{Kind: KindProviderRequest}, ChangedBlobs: oversizedSlice(overLen, hugeHash)})
		}, maxHashBytes, func(pr ProviderRequest) []string { return pr.ChangedBlobs }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line, err := tc.line()
			if err != nil {
				t.Fatalf("Encode() error: %v", err)
			}
			got, err := Decode(line)
			if err != nil {
				t.Fatalf("Decode() error: %v", err)
			}
			pr, ok := got.(ProviderRequest)
			if !ok {
				t.Fatalf("Decode() returned %T, want ProviderRequest", got)
			}
			list := tc.get(pr)
			if n := len(list); n > maxListEntries {
				t.Fatalf("%s len = %d, want <= maxListEntries (%d)", tc.name, n, maxListEntries)
			}
			for i, v := range list {
				if n := len(v); n > tc.entryMax {
					t.Fatalf("%s[%d] len = %d, want <= %d", tc.name, i, n, tc.entryMax)
				}
			}
		})
	}
}

// TestDecodeCapsProviderRequestNewMessages is R5-1 companion coverage for
// ProviderRequest.Summary.NewMessages: Decode must length-cap the slice to
// maxListEntries AND per-field-cap every surviving entry's Role (capID),
// Hash (capHash), and Preview (capField). NewMessages is a []MessageSummary,
// not a []string, so it doesn't fit the shared capStringSlice helper used
// above and gets its own dedicated (not table-driven) test. The input has
// more than maxListEntries entries, each with all three string fields
// individually oversized, so length-capping and per-field-capping are both
// exercised in one decode.
func TestDecodeCapsProviderRequestNewMessages(t *testing.T) {
	overLen := maxListEntries + 50
	hugeRole := strings.Repeat("r", maxIdBytes+1)
	hugeHash := strings.Repeat("h", maxHashBytes+1)
	hugePreview := strings.Repeat("p", maxFieldBytes+1)

	msgs := make([]MessageSummary, overLen)
	for i := range msgs {
		msgs[i] = MessageSummary{Role: hugeRole, Hash: hugeHash, Preview: hugePreview}
	}

	line, err := Encode(ProviderRequest{
		env:     env{Kind: KindProviderRequest},
		Summary: RequestSummary{NewMessages: msgs},
	})
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	got, err := Decode(line)
	if err != nil {
		t.Fatalf("Decode() error: %v", err)
	}
	pr, ok := got.(ProviderRequest)
	if !ok {
		t.Fatalf("Decode() returned %T, want ProviderRequest", got)
	}

	if n := len(pr.Summary.NewMessages); n > maxListEntries {
		t.Fatalf("NewMessages len = %d, want <= maxListEntries (%d)", n, maxListEntries)
	}
	for i, m := range pr.Summary.NewMessages {
		if n := len(m.Role); n > maxIdBytes {
			t.Fatalf("NewMessages[%d].Role len = %d, want <= maxIdBytes (%d)", i, n, maxIdBytes)
		}
		if n := len(m.Hash); n > maxHashBytes {
			t.Fatalf("NewMessages[%d].Hash len = %d, want <= maxHashBytes (%d)", i, n, maxHashBytes)
		}
		if n := len(m.Preview); n > maxFieldBytes {
			t.Fatalf("NewMessages[%d].Preview len = %d, want <= maxFieldBytes (%d)", i, n, maxFieldBytes)
		}
	}
}
