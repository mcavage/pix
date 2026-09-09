package main

import (
	"slices"
	"strings"
	"testing"
)

func TestBarePassthroughSession(t *testing.T) {
	tail := []string{"--session-dir", ".pi-sessions", "--session", "01a086e3-de4f-7098-90fd-7071deb4ddac"}
	root, err := parseRoot(append([]string{"--"}, tail...))
	if err != nil {
		t.Fatal(err)
	}
	opts, err := root.Run.opts()
	if err != nil {
		t.Fatal(err)
	}
	if opts.Workspace != "." || !slices.Equal(opts.Passthrough, tail) {
		t.Fatalf("options = %+v", opts)
	}
	d, _, stderr := rootDeps()
	d.Interactive = false
	if code := dispatch(append([]string{"--"}, tail...), d); code != 2 || !strings.Contains(stderr.String(), "non-interactive") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
