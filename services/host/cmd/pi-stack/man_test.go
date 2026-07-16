package main

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// documentedManVerbs extracts the set of verbs the embedded man page documents
// as INVOCABLE commands: every `"pi-stack <verb>"` quoted command form (used in
// the .BR/.B synopsis lines under each .SS). This deliberately ignores prose
// mentions ("pi-stack binaries", "pi-stack checkout") and reserved stubs that
// carry no invocation form (models/upgrade), so it maps 1:1 onto knownVerbs.
func documentedManVerbs(t *testing.T) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`"pi-stack ([a-z]+)`)
	got := map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(manPage), -1) {
		got[m[1]] = true
	}
	if len(got) == 0 {
		t.Fatal("no `\"pi-stack <verb>` command forms found in embedded man page")
	}
	return got
}

// TestManPageDocumentsEveryKnownVerb is the anti-drift guardrail: a new verb
// added to knownVerbs with no man-page entry fails here (part a), and a verb
// documented in the man page but absent from knownVerbs fails too (part b). This
// keeps the authored page and the dispatch table from silently diverging. The
// page is NOT generated from usage consts — it stays hand-authored, and this
// test is the gate.
func TestManPageDocumentsEveryKnownVerb(t *testing.T) {
	documented := documentedManVerbs(t)

	// (a) every known verb is documented.
	var missing []string
	for v := range knownVerbs {
		if !documented[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("knownVerbs not documented in man page (add a `pi-stack <verb>` entry): %v", missing)
	}

	// (b) the page documents no verb that isn't a known verb.
	var stale []string
	for v := range documented {
		if !knownVerbs[v] {
			stale = append(stale, v)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("man page documents verbs absent from knownVerbs (removed/renamed?): %v", stale)
	}
}

// TestManPageEmbedded proves the roff source is compiled into the binary (the
// go:embed target is in-package) and is non-trivial.
func TestManPageEmbedded(t *testing.T) {
	if len(manPage) == 0 {
		t.Fatal("embedded manPage is empty")
	}
	if !bytes.Contains(manPage, []byte(".TH PI-STACK 1")) {
		t.Error("embedded manPage is missing its .TH header")
	}
}

// TestRenderManNonTTYWritesToStdout proves that with stdout not a TTY, no pager
// is invoked and the page is written straight to the provided writer, so
// `pi-stack man | grep foo` works.
func TestRenderManNonTTYWritesToStdout(t *testing.T) {
	var out bytes.Buffer
	var ran []string
	env := manEnv{
		lookPath: func(name string) (string, error) {
			if name == "mandoc" {
				return "/usr/bin/mandoc", nil
			}
			return "", errors.New("not found")
		},
		getenv: func(string) string { return "" },
		isTTY:  false,
		stdout: &out,
		stderr: io.Discard,
		run: func(_ manEnv, tool string, args []string, pager string, w io.Writer) error {
			ran = append(ran, tool)
			if pager != "" {
				t.Errorf("non-TTY must not page, got pager %q", pager)
			}
			// Simulate mandoc rendering: emit a marker to the writer.
			_, _ = io.WriteString(w, "RENDERED-BY-"+tool)
			return nil
		},
	}
	renderMan(env)
	if len(ran) != 1 || ran[0] != "/usr/bin/mandoc" {
		t.Fatalf("expected mandoc to render once, ran = %v", ran)
	}
	if !strings.Contains(out.String(), "RENDERED-BY-/usr/bin/mandoc") {
		t.Errorf("non-TTY output not written to stdout: %q", out.String())
	}
}

// TestRenderManRawRoffFallback proves that when NO renderer is available and
// stdout is not a TTY, the raw roff still lands on stdout (never an error, never
// an install nag).
func TestRenderManRawRoffFallback(t *testing.T) {
	var out bytes.Buffer
	env := manEnv{
		lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		getenv:   func(string) string { return "" },
		isTTY:    false,
		stdout:   &out,
		stderr:   io.Discard,
		run: func(manEnv, string, []string, string, io.Writer) error {
			t.Fatal("run must not be called when no renderer is found")
			return nil
		},
	}
	renderMan(env)
	if !bytes.Contains(out.Bytes(), []byte(".TH PI-STACK 1")) {
		t.Errorf("raw-roff fallback did not emit the man page; got %d bytes", out.Len())
	}
}

// TestExtractManFlag covers the global --man alias extraction: it is stripped
// from argv, honored before a -- terminator, and left untouched afterward.
func TestExtractManFlag(t *testing.T) {
	cases := []struct {
		argv     []string
		wantRest []string
		wantOK   bool
	}{
		{[]string{"run", "foo"}, []string{"run", "foo"}, false},
		{[]string{"--man"}, nil, true},
		{[]string{"status", "--man"}, []string{"status"}, true},
		{[]string{"--man", "-h"}, []string{"-h"}, true},
		{[]string{"run", "--", "--man"}, []string{"run", "--", "--man"}, false},
	}
	for _, c := range cases {
		rest, ok := extractManFlag(c.argv)
		if ok != c.wantOK {
			t.Errorf("extractManFlag(%v) ok = %v, want %v", c.argv, ok, c.wantOK)
		}
		if strings.Join(rest, "\x00") != strings.Join(c.wantRest, "\x00") {
			t.Errorf("extractManFlag(%v) rest = %v, want %v", c.argv, rest, c.wantRest)
		}
	}
}
