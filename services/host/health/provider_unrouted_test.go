package health

import (
	"context"
	"strings"
	"testing"
)

// fakeKeyStore builds probe args that make runBounded emit a key listing
// containing the given names, so the probe's parsing runs for real.
func fakeKeyStore(names ...string) (string, []string) {
	return "printf", []string{strings.Join(names, "\n") + "\n"}
}

// TestProviderKeyProbeReportsKeysItCannotRoute is the report half of the bug
// that made a fresh install route its top-level session to a vendor nobody
// picked.
//
// `pix doctor` printed "providers  required  anthropic, openai, google" while
// the router could only reach Anthropic, because keys live in the sbx secret
// store and routing resolves over probed bindings. Every line on the screen was
// green and the model choice still looked inexplicable. Ready is still the right
// STATUS (one callable provider is all a launch needs), so the fix is that the
// detail has to say it.
func TestProviderKeyProbeReportsKeysItCannotRoute(t *testing.T) {
	bin, args := fakeKeyStore("anthropic", "openai", "google")
	p := ProviderKeyProbe{
		Bin: bin, Args: args,
		Want:  []string{"anthropic", "openai", "google"},
		AnyOf: true, Label: "providers",
		Callable: []string{"anthropic"},
	}
	r := p.Check(context.Background())
	if r.Status != StatusReady {
		t.Fatalf("status = %v, want ready: one callable provider is enough to launch", r.Status)
	}
	for _, want := range []string{"openai", "google", "no model wired", "pix models add"} {
		if !strings.Contains(r.Detail, want) {
			t.Errorf("detail %q must mention %q", r.Detail, want)
		}
	}
	if !strings.Contains(r.Evidence, "no callable binding") {
		t.Errorf("evidence %q must name the gap it found", r.Evidence)
	}
}

// TestProviderKeyProbeSilentWhenEveryKeyRoutes: no note when there is nothing
// to report. A nag on a correct host is how a real warning gets ignored.
func TestProviderKeyProbeSilentWhenEveryKeyRoutes(t *testing.T) {
	bin, args := fakeKeyStore("anthropic", "openai")
	p := ProviderKeyProbe{
		Bin: bin, Args: args,
		Want:  []string{"anthropic", "openai", "google"},
		AnyOf: true, Label: "providers",
		Callable: []string{"anthropic", "openai"},
	}
	r := p.Check(context.Background())
	if r.Status != StatusReady || strings.Contains(r.Detail, "no model wired") {
		t.Fatalf("clean host must report plainly, got %q (%v)", r.Detail, r.Status)
	}
}

// TestProviderKeyProbeNilCallableIsUnknownNotNone pins the distinction that
// keeps this from fabricating a fault: a host that never ran `pix models add`
// has NO bindings, routes through the baked map, and is fine. nil Callable must
// report exactly what it always did.
func TestProviderKeyProbeNilCallableIsUnknownNotNone(t *testing.T) {
	bin, args := fakeKeyStore("anthropic")
	p := ProviderKeyProbe{
		Bin: bin, Args: args,
		Want:  []string{"anthropic", "openai", "google"},
		AnyOf: true, Label: "providers",
		Callable: nil,
	}
	r := p.Check(context.Background())
	if r.Status != StatusReady {
		t.Fatalf("status = %v, want ready", r.Status)
	}
	if strings.Contains(r.Detail, "no model wired") {
		t.Fatalf("nil Callable means UNKNOWN and must report nothing, got %q", r.Detail)
	}
}
