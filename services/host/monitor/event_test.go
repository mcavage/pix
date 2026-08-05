package monitor

import (
	"encoding/json"
	"reflect"
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
// must reproduce an equal value carrying the same envelope and wire form.
func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, want := range sampleEvents() {
		t.Run(string(want.Envelope().Kind), func(t *testing.T) {
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
	summaryKeys := []string{"systemPromptHash", "systemPromptBytes", "messageCount", "newMessages", "toolCount", "toolNames", "mcpToolNames", "toolSchemaHash", "estTokens"}
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
		if kind == KindProviderRequest {
			var summary map[string]json.RawMessage
			if err := json.Unmarshal(m["summary"], &summary); err != nil {
				t.Fatalf("Unmarshal summary: %v", err)
			}
			for _, k := range summaryKeys {
				if _, ok := summary[k]; !ok {
					t.Errorf("summary missing key %q: %s", k, m["summary"])
				}
			}
		}
		for _, k := range want {
			if _, ok := m[k]; !ok {
				t.Errorf("%s JSON missing key %q: %s", kind, k, line)
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

// longWireFields are the fields allowed maxFieldBytes; every other string is
// an id/label/hash and must come back under maxIDBytes. Kept as data, and
// the input is built off the struct types, so a field ADDED to the wire
// contract is bound-checked by this test without editing it.
var longWireFields = map[string]bool{
	"preview": true, "textPreview": true, "argsSummary": true, "resultSummary": true, "detail": true,
}

// TestDecodeCapsEveryStringBearingField is the retained-memory bound: for
// every kind, decoding a line whose every string is oversized and every list
// over-long must yield nothing above its cap.
func TestDecodeCapsEveryStringBearingField(t *testing.T) {
	for _, sample := range sampleEvents() {
		kind := sample.Envelope().Kind
		t.Run(string(kind), func(t *testing.T) {
			ev, err := Decode(oversizedLine(t, kind, reflect.TypeOf(sample)))
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			checkBounds(t, reflect.ValueOf(ev), "")
		})
	}
	t.Run("unknown kind", func(t *testing.T) {
		line, err := json.Marshal(map[string]any{"kind": "some_future_kind", "detail": hugeText()})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		ev, err := Decode(line)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		checkBounds(t, reflect.ValueOf(ev), "")
	})
	t.Run("multibyte utf8 truncation never panics", func(t *testing.T) {
		// Capping slices bytes, so a 2-byte rune can be cut in half. That
		// must stay a harmless truncation.
		line, err := json.Marshal(map[string]any{
			"kind": KindContextEvent, "ctxKind": "x", "detail": strings.Repeat("é", maxFieldBytes),
		})
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
	})
}

func hugeText() string { return strings.Repeat("x", maxFieldBytes*2) }

// oversizedLine builds a wire line for kind straight off its struct type,
// with every string over maxFieldBytes and every list over maxListEntries.
// List ENTRIES only need to exceed maxIDBytes; repeating a 128KB string 562
// times per list would cost hundreds of MB for no extra coverage.
func oversizedLine(t *testing.T, kind Kind, typ reflect.Type) []byte {
	t.Helper()
	obj := oversizedFields(typ, hugeText())
	obj["kind"] = kind
	line, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("marshal %s: %v", kind, err)
	}
	return line
}

func oversizedFields(typ reflect.Type, text string) map[string]any {
	const entryText = 2 * maxIDBytes
	out := map[string]any{}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch {
		case f.Anonymous && ft.Kind() == reflect.Struct: // embedded envelope: flattened on the wire
			for k, v := range oversizedFields(ft, text) {
				out[k] = v
			}
		case name == "" || name == "-":
		case ft.Kind() == reflect.String:
			out[name] = text
		case ft.Kind() == reflect.Struct:
			out[name] = oversizedFields(ft, text)
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.String:
			out[name] = overLong(strings.Repeat("x", entryText))
		case ft.Kind() == reflect.Slice && ft.Elem().Kind() == reflect.Struct:
			out[name] = overLong(oversizedFields(ft.Elem(), strings.Repeat("x", entryText)))
		}
	}
	return out
}

func overLong[T any](v T) []T {
	list := make([]T, maxListEntries+50)
	for i := range list {
		list[i] = v
	}
	return list
}

// checkBounds walks a decoded event and asserts every retained string, list
// and raw line is within its cap.
func checkBounds(t *testing.T, v reflect.Value, tag string) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		limit := maxIDBytes
		if longWireFields[tag] {
			limit = maxFieldBytes
		}
		if v.Len() > limit {
			t.Errorf("field %q is %d bytes, want <= %d", tag, v.Len(), limit)
		}
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			checkBounds(t, v.Elem(), tag)
		}
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 { // UnknownEvent.Raw
			const maxRaw = maxFieldBytes * 4
			if v.Len() > maxRaw {
				t.Errorf("raw line is %d bytes, want <= %d", v.Len(), maxRaw)
			}
			return
		}
		if v.Len() > maxListEntries {
			t.Errorf("list %q has %d entries, want <= %d", tag, v.Len(), maxListEntries)
		}
		for i := 0; i < v.Len(); i++ {
			checkBounds(t, v.Index(i), tag)
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			name, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("json"), ",")
			checkBounds(t, v.Field(i), name)
		}
	}
}
