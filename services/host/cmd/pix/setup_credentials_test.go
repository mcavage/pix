package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/sys"
)

// setupHome isolates PIX_HOME and puts a fixture `sbx` on PATH that FAILS the
// test if setup's credential step ever runs it: whatever this host holds
// globally is none of setup's business.
func setupHome(t *testing.T) (home string, sbxLog string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("PIX_HOME", home)
	bin := t.TempDir()
	sbxLog = filepath.Join(bin, "sbx.log")
	script := "#!/bin/sh\necho \"$@\" >> " + sbxLog + "\n" +
		"echo 'SCOPE TYPE NAME SECRET'\necho 'global service anthropic ****'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "sbx"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return home, sbxLog
}

// TestSetupCredentials_SeedsRefsDespiteGlobals: setup always establishes THIS
// PIX_HOME's refs file, whatever host-global sbx secrets exist. A host covered
// in globals is not a host that is already set up — it is a host whose
// credentials belong to someone else.
func TestSetupCredentials_SeedsRefsDespiteGlobals(t *testing.T) {
	home, sbxLog := setupHome(t)
	var out, errb bytes.Buffer
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false})

	refs := filepath.Join(home, "secrets.env")
	body, err := os.ReadFile(refs)
	if err != nil {
		t.Fatalf("setup did not create %s: %v", refs, err)
	}
	if !strings.Contains(string(body), "op://") {
		t.Errorf("the seeded refs file carries no commented op:// guidance:\n%s", body)
	}
	if fi, serr := os.Stat(refs); serr == nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("secrets.env mode = %o, want 0600", fi.Mode().Perm())
	}
	if _, serr := os.Stat(sbxLog); serr == nil {
		b, _ := os.ReadFile(sbxLog)
		t.Errorf("setup's credential step consulted sbx: %s", b)
	}

	// Non-interactive: it names the next action, and never claims a model is
	// ready when nothing resolved one.
	got := out.String()
	if !strings.Contains(got, "pix secret set ANTHROPIC_API_KEY op://") {
		t.Errorf("want `pix secret set` named as the next action:\n%s", got)
	}
	for _, forbidden := range []string{"model keys are configured", "ready to run", "verified"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("setup claimed %q with no configured ref:\n%s", forbidden, got)
		}
	}
}

// TestSetupCredentials_ReportsConfiguredRefsWithoutResolving: with a ref
// already configured, setup says so and still resolves nothing — the values
// reach a sandbox at launch, scoped to it, not at setup time.
func TestSetupCredentials_ReportsConfiguredRefsWithoutResolving(t *testing.T) {
	home, sbxLog := setupHome(t)
	if err := os.WriteFile(filepath.Join(home, "secrets.env"),
		[]byte("ANTHROPIC_API_KEY=op://Private/anthropic/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false})

	if !strings.Contains(out.String(), "resolves them into that run's own sandbox") {
		t.Errorf("want the per-sandbox resolution explained:\n%s", out.String())
	}
	if _, serr := os.Stat(sbxLog); serr == nil {
		b, _ := os.ReadFile(sbxLog)
		t.Errorf("setup resolved or pushed a credential: %s", b)
	}
}
