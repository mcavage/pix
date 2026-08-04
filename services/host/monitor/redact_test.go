package monitor

import (
	"strings"
	"testing"
)

// canaryAWSKey is a deliberately fake-but-realistic AWS access key id used
// across these tests (and store_test.go's end-to-end version) as the
// canary: a known secret-shaped token planted in input, whose ABSENCE from
// output is what's actually being proven. If this string ever stopped
// appearing in the redacted output, that would be a false pass, so
// TestRedactTextCanaryIsActuallyMatched below proves the canary is real
// secret-shaped input, not accidentally already-safe text.
const canaryAWSKey = "AKIAABCDEFGHIJKLMNOP"

func TestRedactTextCanaryIsActuallyMatched(t *testing.T) {
	// Sanity check on the canary itself: it must satisfy the AWS-key
	// pattern shape (four letters + 16 uppercase-alnum), or the "redaction
	// worked" tests below would trivially pass for the wrong reason.
	if !strings.HasPrefix(canaryAWSKey, "AKIA") || len(canaryAWSKey) != 20 {
		t.Fatalf("canaryAWSKey %q is not AWS-key-shaped", canaryAWSKey)
	}
}

func TestRedactTextScrubsKnownSecretShapes(t *testing.T) {
	cases := map[string]string{
		"aws":        "export AWS_ACCESS_KEY_ID=" + canaryAWSKey,
		"github":     "token: ghp_1234567890abcdefghijklmnopqrstuvwxyz",
		"slack":      "posted with xoxb-1234567890-abcdefghijklmnop",
		"openai":     "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789",
		"privatekey": "-----BEGIN RSA PRIVATE KEY-----\nMIIB...==\n-----END RSA PRIVATE KEY-----",
		"generickv":  `api_key: "sk_live_abcdefghijklmnopqrstuvwx"`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out := RedactText(in)
			if out == in {
				t.Fatalf("RedactText(%q) was not modified, want the secret redacted", in)
			}
			if !strings.Contains(out, RedactionMarker) {
				t.Errorf("RedactText(%q) = %q, want it to contain %q", in, out, RedactionMarker)
			}
		})
	}
}

func TestRedactTextLeavesOrdinaryTextAlone(t *testing.T) {
	in := "read the file, run go test ./..., then write the summary"
	if out := RedactText(in); out != in {
		t.Errorf("RedactText(%q) = %q, want it unchanged (no secret shape present)", in, out)
	}
}

func TestRedactTextEmptyIsEmpty(t *testing.T) {
	if RedactText("") != "" {
		t.Errorf("RedactText(\"\") = %q, want \"\"", RedactText(""))
	}
}

// TestRedactScrubsEveryFreeTextFieldPerKind proves Redact reaches the
// free-text field of EVERY concrete kind that carries one, using the same
// canary in each field.
func TestRedactScrubsEveryFreeTextFieldPerKind(t *testing.T) {
	e := canaryAWSKey

	cases := []struct {
		name string
		in   Event
		text func(Event) string
	}{
		{"TurnStart.Model", TurnStart{env: sampleEnvelope(KindTurnStart), Model: e},
			func(ev Event) string { return ev.(TurnStart).Model }},
		{"ProviderRequest.NewMessages[0].Preview", ProviderRequest{
			env:     sampleEnvelope(KindProviderRequest),
			Summary: RequestSummary{NewMessages: []MessageSummary{{Preview: e}}},
		}, func(ev Event) string { return ev.(ProviderRequest).Summary.NewMessages[0].Preview }},
		{"ProviderResponse.TextPreview", ProviderResponse{env: sampleEnvelope(KindProviderResponse), TextPreview: e},
			func(ev Event) string { return ev.(ProviderResponse).TextPreview }},
		{"ToolStart.ArgsSummary", ToolStart{env: sampleEnvelope(KindToolStart), ArgsSummary: e},
			func(ev Event) string { return ev.(ToolStart).ArgsSummary }},
		{"ToolEnd.ResultSummary", ToolEnd{env: sampleEnvelope(KindToolEnd), ResultSummary: e},
			func(ev Event) string { return ev.(ToolEnd).ResultSummary }},
		{"ContextEvent.Detail", ContextEvent{env: sampleEnvelope(KindContextEvent), Detail: e},
			func(ev Event) string { return ev.(ContextEvent).Detail }},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Redact(c.in)
			if strings.Contains(c.text(got), canaryAWSKey) {
				t.Fatalf("Redact() left the canary in %s: %v", c.name, got)
			}
		})
	}
}

func TestRedactUnknownEventScrubsRawLine(t *testing.T) {
	line := []byte(`{"kind":"future_kind","sandboxId":"sbx","sessionId":"sess","turnId":"t1","seq":1,"ts":1,"detail":"` + canaryAWSKey + `"}`)
	ev, err := Decode(line)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	redacted := Redact(ev)
	unk, ok := redacted.(UnknownEvent)
	if !ok {
		t.Fatalf("Redact(UnknownEvent) = %T, want UnknownEvent", redacted)
	}
	if strings.Contains(string(unk.Raw), canaryAWSKey) {
		t.Fatalf("Redact(UnknownEvent) left the canary in Raw: %s", unk.Raw)
	}
	// Still valid JSON after redaction (the marker contains no quotes).
	reencoded, err := Encode(unk)
	if err != nil {
		t.Fatalf("Encode(redacted UnknownEvent): %v", err)
	}
	if len(reencoded) == 0 {
		t.Fatal("Encode(redacted UnknownEvent) is empty")
	}
}

func TestRedactKnownKindsWithNoTextFieldAreNoOps(t *testing.T) {
	in := ContextEvent{env: sampleEnvelope(KindContextEvent), CtxKind: "compaction"}
	if got := Redact(in); got != in {
		t.Errorf("Redact() of an event with no free text changed the value: %+v", got)
	}
}
