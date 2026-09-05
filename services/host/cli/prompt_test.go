package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// TestAsk_RetriesARejectedAnswerInPlace: the whole reason Question.Accept
// exists. A typo is corrected at the same prompt; the caller is not sent off
// to run a separate command and start the step over.
func TestAsk_RetriesARejectedAnswerInPlace(t *testing.T) {
	var out bytes.Buffer
	d := &Deps{Out: &out, Interactive: true, In: strings.NewReader("nope\nop://Private/Item/field\n")}
	var recorded []string
	got, ok := d.Ask(Question{
		Label:   "ANTHROPIC_API_KEY",
		Example: "op://Private/Item/field",
		Accept: func(v string) error {
			recorded = append(recorded, v)
			if !strings.HasPrefix(v, "op://") {
				return fmt.Errorf("not an op:// reference")
			}
			return nil
		},
	})
	if !ok || got != "op://Private/Item/field" {
		t.Fatalf("Ask = (%q, %v), want the corrected value accepted", got, ok)
	}
	if len(recorded) != 2 {
		t.Errorf("Accept calls = %v, want the rejected answer then the corrected one", recorded)
	}
	if !strings.Contains(out.String(), "not an op:// reference") || !strings.Contains(out.String(), "example: op://Private/Item/field") {
		t.Errorf("output = %q, want the rejection reason and the example", out.String())
	}
}

// TestAsk_EnterKeepsTheCurrentValue: an editable prompt shows what is
// already recorded and treats a bare Enter as "keep it", never as "clear
// it".
func TestAsk_EnterKeepsTheCurrentValue(t *testing.T) {
	var out bytes.Buffer
	d := &Deps{Out: &out, Interactive: true, In: strings.NewReader("\n")}
	got, ok := d.Ask(Question{Label: "GOG_ACCOUNT", Current: "mark@docker.com"})
	if !ok || got != "mark@docker.com" {
		t.Fatalf("Ask = (%q, %v), want the current value kept", got, ok)
	}
	if !strings.Contains(out.String(), "[mark@docker.com]") {
		t.Errorf("output = %q, want the current value offered as the default", out.String())
	}
}

// TestAsk_SkipsWithoutAnAsker: no TTY, a blank answer with nothing
// recorded, and an exhausted attempt budget are all "still not recorded" —
// the caller must print its own remedy, so none of them may report ok.
func TestAsk_SkipsWithoutAnAsker(t *testing.T) {
	cases := []struct {
		name string
		d    *Deps
		q    Question
	}{
		{"non-interactive", &Deps{Out: &bytes.Buffer{}, In: strings.NewReader("value\n")}, Question{Label: "K"}},
		{"blank answer", &Deps{Out: &bytes.Buffer{}, Interactive: true, In: strings.NewReader("\n")}, Question{Label: "K"}},
		{"eof", &Deps{Out: &bytes.Buffer{}, Interactive: true, In: strings.NewReader("")}, Question{Label: "K"}},
		{"budget exhausted", &Deps{Out: &bytes.Buffer{}, Interactive: true, In: strings.NewReader("a\nb\nc\nd\n")}, Question{
			Label:  "K",
			Accept: func(string) error { return fmt.Errorf("always wrong") },
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := tc.d.Ask(tc.q); ok {
				t.Errorf("Ask = (%q, true), want a skip", got)
			}
		})
	}
}

// TestDepsLine_OnePromptNeverEatsTheNextOne is the regression this shared
// reader exists for: a bufio reader created PER PROMPT buffers whatever is
// available and discards the remainder, so with several prompts across
// several functions in one `pix setup` run, everything after the first
// answer disappeared. All reads must come from Deps.Line.
func TestDepsLine_OnePromptNeverEatsTheNextOne(t *testing.T) {
	var out bytes.Buffer
	d := &Deps{Out: &out, Interactive: true, In: strings.NewReader("first\ny\nsecond\n")}
	one, ok := d.Ask(Question{Label: "ONE"})
	if !ok || one != "first" {
		t.Fatalf("first Ask = (%q, %v)", one, ok)
	}
	if !d.AskYN("continue? [y/N] ", false) {
		t.Fatal("AskYN did not see the typed-ahead y")
	}
	two, ok := d.Ask(Question{Label: "TWO"})
	if !ok || two != "second" {
		t.Fatalf("second Ask = (%q, %v), want the line typed ahead of the first prompt", two, ok)
	}
}

// TestLineIn_ReusesAnAlreadyBufferedReader: the property that lets a helper
// keep taking an io.Reader (secret.OfferOnePasswordKeys and friends) and
// still share the command's one buffer.
func TestLineIn_ReusesAnAlreadyBufferedReader(t *testing.T) {
	d := &Deps{Out: &bytes.Buffer{}, Interactive: true, In: strings.NewReader("x\ny\n")}
	shared := d.Line()
	if LineIn(shared) != shared {
		t.Error("LineIn wrapped an existing *bufio.Reader instead of reusing it")
	}
}
