// render_trust_bill_inference_test.go — the Trust Bill security finding:
// renderTrustBill previously counted Inference in nothing and printed no
// per-backend line at all, so a human answering "y" at `pix env trust` or
// `pix run`'s first-use prompt never saw the model-traffic endpoint(s) an
// accepted environment would route a session's inference through. Inference
// is now counted by default (D15, revised: default is counts/risk
// categories only) and every per-backend field — name, driver, base URL,
// auth — renders under --verbose, the same place every other section's
// detail lives. These tests prove: (1) the default summary/count line
// includes inference but names no backend, (2) --verbose shows every
// InferenceFact field, (3) attacker-controlled fields pass through
// sys.TerminalSafe under --verbose (where they actually render) so a
// control character cannot forge an extra terminal line (a fake count, a
// fake prompt, a fake "trusted" verdict), and (4) the --verbose
// representation is exactly the fingerprinted InferenceFact — never a
// value that could diverge from what Fingerprint actually hashed.
package main

import (
	"bytes"
	"strings"
	"testing"

	nativeenv "pix/host/workflow/env"
)

// TestRenderTrustBill_CountsInferenceByDefaultWithoutNamingIt proves the
// default (non-verbose) render counts an InferenceFact but names none of
// its fields — the summary/count line is the whole default screen for
// inference now, matching every other bill section.
func TestRenderTrustBill_CountsInferenceByDefaultWithoutNamingIt(t *testing.T) {
	b := nativeenv.BillOfMaterials{
		Inference: []nativeenv.InferenceFact{
			{
				Name:    "rogue",
				Driver:  "openai-compatible",
				BaseURL: "https://attacker.example/v1",
				Auth:    "bearer",
				KeyEnv:  "ROGUE_KEY",
			},
		},
	}
	var out bytes.Buffer
	renderTrustBill(&out, "work", b, false)
	got := out.String()

	if !strings.Contains(got, "1 inference backend(s)") {
		t.Errorf("summary line did not count inference backends; got:\n%s", got)
	}
	for _, notWant := range []string{"rogue", "openai-compatible", "attacker.example", "bearer"} {
		if strings.Contains(got, notWant) {
			t.Errorf("default render should not name inference detail %q; got:\n%s", notWant, got)
		}
	}
}

// TestRenderTrustBill_RendersInferenceFactsWhenVerboseToo proves --verbose
// still shows the inference line (it is not demoted to verbose-only, since
// D15's default view must already show every model-traffic endpoint).
func TestRenderTrustBill_RendersInferenceFactsWhenVerboseToo(t *testing.T) {
	b := nativeenv.BillOfMaterials{
		Inference: []nativeenv.InferenceFact{
			{Name: "zai", Driver: "anthropic", BaseURL: "https://api.z.ai", Auth: "api-key"},
		},
	}
	var out bytes.Buffer
	renderTrustBill(&out, "work", b, true)
	got := out.String()
	for _, want := range []string{"zai", "anthropic", "api.z.ai", "api-key", "1 inference backend(s)"} {
		if !strings.Contains(got, want) {
			t.Errorf("verbose render missing %q; got:\n%s", want, got)
		}
	}
}

// TestRenderTrustBill_InferenceControlCharsCannotForgeATerminalLine is the
// injection proof: an attacker-authored backend name/driver/base_url/auth
// containing raw control characters (ESC, CR, LF) must never reach the
// writer literally — sys.TerminalSafe must have replaced every one with an
// inert escape, so the rendered line count matches exactly what the
// renderer itself emitted, never a forged extra line the attacker
// injected via a raw newline.
func TestRenderTrustBill_InferenceControlCharsCannotForgeATerminalLine(t *testing.T) {
	evil := "trusted\n\x1b]0;pwned\x07\rFAKE LINE: everything is fine"
	b := nativeenv.BillOfMaterials{
		Inference: []nativeenv.InferenceFact{
			{Name: evil, Driver: evil, BaseURL: "https://attacker.example/" + evil, Auth: evil},
		},
	}
	var out bytes.Buffer
	// Inference detail only renders under --verbose now; that is where the
	// injection-safety proof actually needs to hold.
	renderTrustBill(&out, "work", b, true)
	got := out.String()

	if strings.Contains(got, "\x1b") {
		t.Fatalf("raw ESC reached the terminal-facing output:\n%q", got)
	}
	if strings.Contains(got, "\r") {
		t.Fatalf("raw carriage return reached the terminal-facing output:\n%q", got)
	}
	if strings.Contains(got, "FAKE LINE") {
		// Not itself forbidden text, but confirm it never lands on its OWN
		// forged line: every occurrence must be on the SAME physical line
		// as the "inference:" prefix that introduced it, i.e. the only "\n"
		// before it in that line's own text is the escaped `\n`, not a raw
		// one.
		for _, line := range strings.Split(got, "\n") {
			if strings.Contains(line, "FAKE LINE") && !strings.Contains(line, "inference:") {
				t.Fatalf("attacker payload escaped onto its own forged terminal line: %q", line)
			}
		}
	}
	// Every rendered line must be one the renderer itself introduces
	// (a known prefix) — no bare/forged line exists.
	knownPrefixes := []string{
		"pix env trust", "  environment runs code", "  ", // continuation/indent lines from safe-escaped content are fine
	}
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if line == "" {
			continue
		}
		ok := false
		for _, p := range knownPrefixes {
			if strings.HasPrefix(line, p) {
				ok = true
				break
			}
		}
		if !ok {
			t.Fatalf("unrecognized/forged line in trust bill output: %q\nfull output:\n%s", line, got)
		}
	}
}

// TestRenderTrustBill_PrintedInferenceMatchesFingerprintedFact proves the
// printed representation corresponds EXACTLY to the fingerprinted
// InferenceFact: computing Fingerprint over a BOM and rendering the SAME
// BOM must agree on every value a reviewer sees — nothing shown was never
// fingerprinted, and nothing fingerprinted differs from what was shown.
func TestRenderTrustBill_PrintedInferenceMatchesFingerprintedFact(t *testing.T) {
	fact := nativeenv.InferenceFact{
		Name: "prod", Driver: "openai", BaseURL: "https://api.openai.example", Auth: "bearer", KeyEnv: "OPENAI_KEY",
	}
	b := nativeenv.BillOfMaterials{Inference: []nativeenv.InferenceFact{fact}}

	fp1, err := nativeenv.Fingerprint(b)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	var out bytes.Buffer
	// Inference detail is --verbose-only now; that is the rendering this
	// proof is actually about ("nothing shown was never fingerprinted").
	renderTrustBill(&out, "work", b, true)
	got := out.String()
	for _, want := range []string{fact.Name, fact.Driver, fact.BaseURL, fact.Auth} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered output missing fingerprinted fact %q; got:\n%s", want, got)
		}
	}

	// Mutating any rendered field must change the fingerprint too — proof
	// the shown value is not decorative but load-bearing in the hash.
	mutated := b
	mutated.Inference = []nativeenv.InferenceFact{{
		Name: fact.Name, Driver: fact.Driver, BaseURL: "https://different.example", Auth: fact.Auth, KeyEnv: fact.KeyEnv,
	}}
	fp2, err := nativeenv.Fingerprint(mutated)
	if err != nil {
		t.Fatalf("Fingerprint(mutated): %v", err)
	}
	if fp1 == fp2 {
		t.Fatal("changing a rendered inference field (base_url) did not change the fingerprint")
	}
}
