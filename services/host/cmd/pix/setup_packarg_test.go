// setup_packarg_test.go — pack.NormalizeSetupPackArg is setup's argument handling,
// not pack's.
package main

import (
	"pix/host/workflow/pack"
	"testing"
)

func TestNormalizeSetupPackArg(t *testing.T) {
	if got := pack.NormalizeSetupPackArg("acme/work-pack"); got != "https://github.com/acme/work-pack.git" {
		t.Fatalf("got %q", got)
	}
	for _, unchanged := range []string{"./local", "/tmp/local", "https://github.com/a/b.git", "git@github.com:a/b.git"} {
		if got := pack.NormalizeSetupPackArg(unchanged); got != unchanged {
			t.Fatalf("normalize %q = %q", unchanged, got)
		}
	}
}
