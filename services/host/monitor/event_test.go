package monitor

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleEnvelope(kind Kind) env {
	return env{Kind: kind, SandboxID: "sbx-1", SessionID: "sess-1", TurnID: "turn-1", Seq: 7, TS: 1700000000000}
}

func sampleEvents() []Event {
	return []Event{
		TurnStart{env: sampleEnvelope(KindTurnStart), Model: "claude-opus-5", Trigger: "user"},
		ProviderRequest{
			env:   sampleEnvelope(KindProviderRequest),
			Model: "claude-opus-5",
			Summary: RequestSummary{
				SystemPromptHash: "h1", SystemPromptBytes: 12, MessageCount: 2,
				NewMessages:    []MessageSummary{{Role: "user", Bytes: 3, Hash: "h2", Preview: "hi"}},
				ToolCount:      2,
				ToolNames:      []string{"bash", "read"},
				McpToolNames:   []string{"slack_post"},
				ToolSchemaHash: "h3", EstTokens: 99,
			},
			ChangedBlobs: []string{"h1", "h2"},
			Trigger:      "user",
		},
		ProviderResponse{
			env: sampleEnvelope(KindProviderResponse), Status: 200, StopReason: "end_turn",
			Usage:     &UsageSummary{InputTokens: 10, OutputTokens: 20, TotalTokens: 30},
			TextBytes: 5, TextPreview: "hello", TextHash: "h4", ToolCalls: []string{"bash"},
		},
		ToolStart{
			env: sampleEnvelope(KindToolStart), ToolID: "t1", Source: "builtin", Name: "bash",
			ArgsSummary: "ls -l", ArgsHash: "h5", InvokesPi: true,
		},
		ToolEnd{
			env: sampleEnvelope(KindToolEnd), ToolID: "t1", OK: true,
			ResultBytes: 42, ResultSummary: "done", ResultHash: "h6", DurationMs: 17,
		},
		ContextEvent{env: sampleEnvelope(KindContextEvent), CtxKind: "compaction", Detail: "threshold"},
	}
}

// TestEncodeDecodeRoundTrip covers every concrete kind: Encode then Decode
// must reproduce an equal value carrying the same envelope.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, want := range sampleEvents() {
		kind := want.Envelope().Kind
		t.Run(string(kind), func(t *testing.T) {
			line, err := Encode(want)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			got, err := Decode(line)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if got.Envelope() != want.Envelope() {
				t.Fatalf("envelope = %+v, want %+v", got.Envelope(), want.Envelope())
			}
			regot, err := Encode(got)
			if err != nil {
				t.Fatalf("re-Encode: %v", err)
			}
			if string(regot) != string(line) {
				t.Fatalf("round-trip changed the wire form:\n got %s\nwant %s", regot, line)
			}
		})
	}
}

// TestWireSchemaGolden pins the exact JSON the tap must produce and this
// package must accept. A renamed or dropped key here is a wire break with
// extensions/monitor.ts, which is why the field names are asserted literally
// rather than by round-tripping our own structs.
func TestWireSchemaGolden(t *testing.T) {
	line, err := Encode(TurnStart{
		env:   env{Kind: KindTurnStart, SandboxID: "sbx", SessionID: "sess", TurnID: "t1", Seq: 3, TS: 1234},
		Model: "opus", Trigger: "user",
	})
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := `{"kind":"turn_start","sandboxId":"sbx","sessionId":"sess","turnId":"t1","seq":3,"ts":1234,"model":"opus","trigger":"user"}`
	if string(line) != want {
		t.Fatalf("turn_start JSON =\n  %s\nwant\n  %s", line, want)
	}

	keys := map[Kind][]string{
		KindProviderRequest:  {"kind", "sandboxId", "sessionId", "turnId", "seq", "ts", "model", "summary", "changedBlobs", "trigger"},
		KindProviderResponse: {"kind", "status", "stopReason", "usage", "textBytes", "textPreview", "textHash", "toolCalls"},
		KindToolStart:        {"toolId", "source", "name", "argsSummary", "argsHash", "invokesPi"},
		KindToolEnd:          {"toolId", "ok", "resultBytes", "resultSummary", "resultHash", "durationMs"},
		KindContextEvent:     {"ctxKind", "detail"},
	}
	for _, e := range sampleEvents() {
		kind := e.Envelope().Kind
		want, ok := keys[kind]
		if !ok {
			continue
		}
		line, err := Encode(e)
		if err != nil {
			t.Fatalf("Encode %s: %v", kind, err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("Unmarshal %s: %v", kind, err)
		}
		for _, k := range want {
			if _, ok := m[k]; !ok {
				t.Errorf("%s JSON missing key %q: %s", kind, k, line)
			}
		}
		if kind == KindProviderRequest {
			var summary map[string]json.RawMessage
			if err := json.Unmarshal(m["summary"], &summary); err != nil {
				t.Fatalf("Unmarshal summary: %v", err)
			}
			for _, k := range []string{"systemPromptHash", "systemPromptBytes", "messageCount", "newMessages", "toolCount", "toolNames", "mcpToolNames", "toolSchemaHash", "estTokens"} {
				if _, ok := summary[k]; !ok {
					t.Errorf("summary missing key %q: %s", k, m["summary"])
				}
			}
		}
	}
}

func TestDecodeRejectsMalformedAndDataOnlyLines(t *testing.T) {
	cases := map[string]string{
		"malformed json": `{"kind":`,
		"missing kind":   `{"sandboxId":"s"}`,
		"empty kind":     `{"kind":"","sandboxId":"s"}`,
		"blob is data-only, never on the event stream": `{"kind":"blob","hash":"h","text":"t"}`,
	}
	for name, line := range cases {
		if _, err := Decode([]byte(line)); err == nil {
			t.Errorf("Decode(%s) = nil error, want an error", name)
		}
	}
}

// TestDecodeUnknownKindIsForwardCompatible: a NEWER tap's unrecognized kind
// must survive as an UnknownEvent carrying the whole original line, not be
// rejected.
func TestDecodeUnknownKindIsForwardCompatible(t *testing.T) {
	line := `{"kind":"future_kind","sandboxId":"sbx","sessionId":"sess","turnId":"t","seq":9,"ts":5,"extra":{"a":1}}`
	ev, err := Decode([]byte(line))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	unknown, ok := ev.(UnknownEvent)
	if !ok {
		t.Fatalf("Decode returned %T, want UnknownEvent", ev)
	}
	if got := unknown.Envelope(); got.Kind != "future_kind" || got.SandboxID != "sbx" || got.Seq != 9 {
		t.Fatalf("envelope = %+v, want the decoded fields", got)
	}
	back, err := Encode(unknown)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(back) != line {
		t.Fatalf("Encode(UnknownEvent) = %s, want the original line verbatim", back)
	}
}

// TestDecodeCapsEveryStringBearingField is the retained-memory bound: no
// decoded string, and no decoded slice, may exceed its cap, whatever the tap
// sends. One oversized value per field, checked on one event per kind.
func TestDecodeCapsEveryStringBearingField(t *testing.T) {
	huge := strings.Repeat("x", maxFieldBytes*2)
	// List entries only need to exceed maxIDBytes; repeating the 128KB
	// string 562 times per list would make this test cost hundreds of MB of
	// JSON for no extra coverage.
	long := strings.Repeat("x", maxIDBytes*2)
	hugeList := make([]string, maxListEntries+50)
	for i := range hugeList {
		hugeList[i] = long
	}
	envJSON := func(kind Kind, extra map[string]any) []byte {
		obj := map[string]any{"kind": kind, "sandboxId": huge, "sessionId": huge, "turnId": huge, "seq": 1, "ts": 2}
		for k, v := range extra {
			obj[k] = v
		}
		line, err := json.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return line
	}
	check := func(what, s string, limit int) {
		if len(s) > limit {
			t.Errorf("%s is %d bytes, want <= %d", what, len(s), limit)
		}
	}
	checkList := func(what string, list []string, limit int) {
		if len(list) > maxListEntries {
			t.Errorf("%s has %d entries, want <= %d", what, len(list), maxListEntries)
		}
		for _, s := range list {
			check(what+" entry", s, limit)
		}
	}
	checkEnvelope := func(e Envelope) {
		check("sandboxId", e.SandboxID, maxIDBytes)
		check("sessionId", e.SessionID, maxIDBytes)
		check("turnId", e.TurnID, maxIDBytes)
	}

	decode := func(kind Kind, extra map[string]any) Event {
		ev, err := Decode(envJSON(kind, extra))
		if err != nil {
			t.Fatalf("Decode %s: %v", kind, err)
		}
		checkEnvelope(ev.Envelope())
		return ev
	}

	ts := decode(KindTurnStart, map[string]any{"model": huge, "trigger": huge}).(TurnStart)
	check("turn_start.model", ts.Model, maxIDBytes)
	check("turn_start.trigger", ts.Trigger, maxIDBytes)

	pr := decode(KindProviderRequest, map[string]any{
		"model": huge, "trigger": huge, "changedBlobs": hugeList,
		"summary": map[string]any{
			"systemPromptHash": huge, "toolSchemaHash": huge,
			"toolNames": hugeList, "mcpToolNames": hugeList,
			"newMessages": func() []any {
				var out []any
				for i := 0; i < maxListEntries+50; i++ {
					out = append(out, map[string]any{"role": long, "hash": long, "preview": long})
				}
				return out
			}(),
		},
	}).(ProviderRequest)
	check("provider_request.model", pr.Model, maxIDBytes)
	check("summary.systemPromptHash", pr.Summary.SystemPromptHash, maxIDBytes)
	check("summary.toolSchemaHash", pr.Summary.ToolSchemaHash, maxIDBytes)
	checkList("summary.toolNames", pr.Summary.ToolNames, maxIDBytes)
	checkList("summary.mcpToolNames", pr.Summary.McpToolNames, maxIDBytes)
	checkList("changedBlobs", pr.ChangedBlobs, maxIDBytes)
	if len(pr.Summary.NewMessages) > maxListEntries {
		t.Errorf("summary.newMessages has %d entries, want <= %d", len(pr.Summary.NewMessages), maxListEntries)
	}
	for _, m := range pr.Summary.NewMessages {
		check("newMessages.role", m.Role, maxIDBytes)
		check("newMessages.hash", m.Hash, maxIDBytes)
		check("newMessages.preview", m.Preview, maxFieldBytes)
	}

	presp := decode(KindProviderResponse, map[string]any{
		"stopReason": huge, "textPreview": huge, "textHash": huge, "toolCalls": hugeList,
	}).(ProviderResponse)
	check("provider_response.stopReason", presp.StopReason, maxIDBytes)
	check("provider_response.textPreview", presp.TextPreview, maxFieldBytes)
	check("provider_response.textHash", presp.TextHash, maxIDBytes)
	checkList("provider_response.toolCalls", presp.ToolCalls, maxIDBytes)

	tstart := decode(KindToolStart, map[string]any{
		"toolId": huge, "source": huge, "name": huge, "argsSummary": huge, "argsHash": huge,
	}).(ToolStart)
	check("tool_start.toolId", tstart.ToolID, maxIDBytes)
	check("tool_start.source", tstart.Source, maxIDBytes)
	check("tool_start.name", tstart.Name, maxIDBytes)
	check("tool_start.argsSummary", tstart.ArgsSummary, maxFieldBytes)
	check("tool_start.argsHash", tstart.ArgsHash, maxIDBytes)

	tend := decode(KindToolEnd, map[string]any{"toolId": huge, "resultSummary": huge, "resultHash": huge}).(ToolEnd)
	check("tool_end.toolId", tend.ToolID, maxIDBytes)
	check("tool_end.resultSummary", tend.ResultSummary, maxFieldBytes)
	check("tool_end.resultHash", tend.ResultHash, maxIDBytes)

	ctx := decode(KindContextEvent, map[string]any{"ctxKind": huge, "detail": huge}).(ContextEvent)
	check("context_event.ctxKind", ctx.CtxKind, maxIDBytes)
	check("context_event.detail", ctx.Detail, maxFieldBytes)

	unknown := decode("some_future_kind", map[string]any{"detail": huge}).(UnknownEvent)
	if len(unknown.Raw) > maxFieldBytes*4 {
		t.Errorf("UnknownEvent.Raw is %d bytes, want <= %d", len(unknown.Raw), maxFieldBytes*4)
	}
}

// TestCapTruncationNeverPanicsOnMultibyteUTF8: capping is a byte slice, so a
// multi-byte rune can be cut in half. That must stay a harmless truncation.
func TestCapTruncationNeverPanicsOnMultibyteUTF8(t *testing.T) {
	detail := strings.Repeat("é", maxFieldBytes) // 2 bytes per rune
	line, err := json.Marshal(map[string]any{"kind": KindContextEvent, "ctxKind": "x", "detail": detail})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	ev, err := Decode(line)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got := len(ev.(ContextEvent).Detail); got > maxFieldBytes {
		t.Fatalf("detail is %d bytes, want <= %d", got, maxFieldBytes)
	}
}
