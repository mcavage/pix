package man

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"pix/host/service"
	"regexp"
	"strings"
	"testing"
)

// TestManPageEmbedded proves the roff source is compiled into the binary (the
// embed target is in-package) and is non-trivial.
func TestManPageEmbedded(t *testing.T) {
	if len(manPage) == 0 {
		t.Fatal("embedded manPage is empty")
	}
	if !bytes.Contains(manPage, []byte(".TH PIX 1")) {
		t.Error("embedded manPage is missing its .TH header")
	}
}

// TestRenderManNonTTYWritesToStdout proves that with stdout not a TTY, no pager
// is invoked and the page is written straight to the provided writer, so
// `pix man | grep foo` works.
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
	if !bytes.Contains(out.Bytes(), []byte(".TH PIX 1")) {
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
		rest, ok := ExtractManFlag(c.argv)
		if ok != c.wantOK {
			t.Errorf("ExtractManFlag(%v) ok = %v, want %v", c.argv, ok, c.wantOK)
		}
		if strings.Join(rest, "\x00") != strings.Join(c.wantRest, "\x00") {
			t.Errorf("ExtractManFlag(%v) rest = %v, want %v", c.argv, rest, c.wantRest)
		}
	}
}

// serveSubverbsFromUsage parses the subverb names out of service.Usage's
// `subcommands:` block (two-space-indented leading token), the same
// single-source-of-truth pattern configKeysFromHelp uses.
func serveSubverbsFromUsage(t *testing.T) []string {
	t.Helper()
	block := service.Usage
	if i := strings.Index(block, "subcommands:"); i >= 0 {
		block = block[i:]
	}
	re := regexp.MustCompile(`(?m)^  ([a-z]+) `)
	seen := map[string]bool{}
	var subs []string
	for _, m := range re.FindAllStringSubmatch(block, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			subs = append(subs, m[1])
		}
	}
	if len(subs) == 0 {
		t.Fatal("no subverbs parsed from service.Usage")
	}
	return subs
}
