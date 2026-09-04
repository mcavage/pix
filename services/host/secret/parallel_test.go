package secret

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// ConfiguredParallelSearchRef must answer from THIS PIX_HOME's refs file
// alone: a filled ref is configured, a placeholder or missing ref is not,
// and a ref for a different key never counts.
func TestConfiguredParallelSearchRef(t *testing.T) {
	cases := []struct {
		name    string
		content string
		exists  bool
		want    bool
	}{
		{"filled ref", "PARALLEL_API_KEY=op://v/parallel/key\n", true, true},
		{"placeholder ref", "PARALLEL_API_KEY=op://<vault>/<item>/<field>\n", true, false},
		{"no refs file", "", false, false},
		{"unrelated ref only", "ANTHROPIC_API_KEY=op://v/a/k\n", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) {
				if !c.exists {
					return "", os.ErrNotExist
				}
				return c.content, nil
			}}}
			if got := ConfiguredParallelSearchRef(env); got != c.want {
				t.Errorf("ConfiguredParallelSearchRef() = %v, want %v", got, c.want)
			}
		})
	}
}

// OfferParallelSearchKey must stay silent unless TTY AND no ref is already
// configured — never a nag on a second run, never a prompt off a script.
func TestOfferParallelSearchKey_Gating(t *testing.T) {
	blank := func() hostenv.Env {
		return hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "", os.ErrNotExist }}}
	}

	// not a tty -> silent
	var out bytes.Buffer
	OfferParallelSearchKey(blank(), strings.NewReader("y\nop://v/parallel/key\n"), &out, false)
	if out.String() != "" {
		t.Errorf("must be silent when not a tty, got %q", out.String())
	}

	// already configured -> silent even on a tty
	out.Reset()
	configured := hostenv.Env{System: &systest.Fake{ReadFileFn: func(string) (string, error) { return "PARALLEL_API_KEY=op://v/parallel/key\n", nil }}}
	OfferParallelSearchKey(configured, strings.NewReader("y\n"), &out, true)
	if out.String() != "" {
		t.Errorf("must be silent when a ref is already configured, got %q", out.String())
	}

	// nil reader (non-interactive path with no stdin at all) -> silent
	out.Reset()
	OfferParallelSearchKey(blank(), nil, &out, true)
	if out.String() != "" {
		t.Errorf("must be silent with a nil reader, got %q", out.String())
	}
}

// Declining (bare Enter, the default) must leave secrets.env untouched, and
// must never claim it was configured.
func TestOfferParallelSearchKey_DeclineWritesNothing(t *testing.T) {
	wrote := false
	env := hostenv.Env{System: &systest.Fake{
		ReadFileFn:  func(string) (string, error) { return "", os.ErrNotExist },
		WriteFileFn: func(string, []byte, os.FileMode) error { wrote = true; return nil },
	}}
	var out bytes.Buffer
	OfferParallelSearchKey(env, strings.NewReader("\n"), &out, true)
	if wrote {
		t.Error("declining must not write secrets.env")
	}
	if strings.Contains(out.String(), "Saved") {
		t.Errorf("declining must not claim it saved anything: %q", out.String())
	}
}

// Accepting and pasting a valid op:// ref writes exactly that ref under
// PARALLEL_API_KEY, and the output never carries a secret VALUE (there is
// none in this flow — only the ref itself, which is not a credential).
func TestOfferParallelSearchKey_AcceptWritesRef(t *testing.T) {
	var written string
	env := hostenv.Env{System: &systest.Fake{
		ReadFileFn:  func(string) (string, error) { return "", os.ErrNotExist },
		WriteFileFn: func(_ string, data []byte, _ os.FileMode) error { written = string(data); return nil },
	}}
	var out bytes.Buffer
	OfferParallelSearchKey(env, strings.NewReader("y\nop://Docker/parallel/key\n"), &out, true)
	if !strings.Contains(written, "PARALLEL_API_KEY=op://Docker/parallel/key") {
		t.Errorf("expected the pasted ref written under PARALLEL_API_KEY, got %q", written)
	}
	if !strings.Contains(out.String(), "Saved") {
		t.Errorf("expected a saved confirmation, got %q", out.String())
	}
}

// A non-op:// paste is rejected, not silently accepted as a literal secret.
func TestOfferParallelSearchKey_RejectsNonOpRef(t *testing.T) {
	wrote := false
	env := hostenv.Env{System: &systest.Fake{
		ReadFileFn:  func(string) (string, error) { return "", os.ErrNotExist },
		WriteFileFn: func(string, []byte, os.FileMode) error { wrote = true; return nil },
	}}
	var out bytes.Buffer
	OfferParallelSearchKey(env, strings.NewReader("y\nsk-live-not-a-ref\n"), &out, true)
	if wrote {
		t.Error("a non-op:// paste must never be written to secrets.env")
	}
	if !strings.Contains(out.String(), "not an op:// ref") {
		t.Errorf("expected the rejection to be explained, got %q", out.String())
	}
}
