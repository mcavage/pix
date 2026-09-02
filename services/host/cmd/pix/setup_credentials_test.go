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

	// Parallel web search is optional and unconfigured: setup says so
	// accurately, names the exact fix, and never claims it is configured.
	if !strings.Contains(got, "Parallel web search is optional and not configured") {
		t.Errorf("want the Parallel web-search fallback state explained:\n%s", got)
	}
	if !strings.Contains(got, "pix secret set PARALLEL_API_KEY op://") {
		t.Errorf("want `pix secret set PARALLEL_API_KEY` named as the fix:\n%s", got)
	}
	if strings.Contains(got, "Parallel web search is configured") {
		t.Errorf("setup claimed Parallel search is configured with no ref present:\n%s", got)
	}
}

// TestSetupCredentials_ReportsParallelSearchConfigured: a filled
// PARALLEL_API_KEY ref is reported as configured, and setup still never
// resolves or shells out to consult it (sbxLog stays empty per setupHome's
// fixture, which fails the test if sbx were ever invoked).
func TestSetupCredentials_ReportsParallelSearchConfigured(t *testing.T) {
	home, _ := setupHome(t)
	if err := os.WriteFile(filepath.Join(home, "secrets.env"),
		[]byte("ANTHROPIC_API_KEY=op://Private/anthropic/key\nPARALLEL_API_KEY=op://Private/parallel/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false})

	got := out.String()
	if !strings.Contains(got, "Parallel web search is configured") {
		t.Errorf("want Parallel search reported as configured:\n%s", got)
	}
	if strings.Contains(got, "Parallel web search is optional and not configured") {
		t.Errorf("setup claimed the fallback state with a ref present:\n%s", got)
	}
}

// TestSetupCredentials_InteractiveOfferWritesRefAndReportsConfigured proves
// the TTY path end to end through setupCredentials itself, not just the
// secret package helper: accepting the offer and pasting a ref leaves
// secrets.env with that ref and the SAME run's report already says
// configured.
// A provider ref is already seeded so the 1Password model-key offer's own
// gate (ProviderKeyRefsPresent) skips it deterministically REGARDLESS of
// whether this test host happens to have `op` on PATH — the first line of
// input is then guaranteed to reach the Parallel offer, not a race against
// this host's own tooling.
func TestSetupCredentials_InteractiveOfferWritesRefAndReportsConfigured(t *testing.T) {
	home, sbxLog := setupHome(t)
	if err := os.WriteFile(filepath.Join(home, "secrets.env"),
		[]byte("ANTHROPIC_API_KEY=op://Private/anthropic/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader("y\nop://Docker/parallel/key\n"), Interactive: true})

	body, err := os.ReadFile(filepath.Join(home, "secrets.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "PARALLEL_API_KEY=op://Docker/parallel/key") {
		t.Errorf("want the accepted ref written to secrets.env:\n%s", body)
	}
	if !strings.Contains(out.String(), "Parallel web search is configured") {
		t.Errorf("want the same run to report it configured after accepting:\n%s", out.String())
	}
	if _, serr := os.Stat(sbxLog); serr == nil {
		b, _ := os.ReadFile(sbxLog)
		t.Errorf("the Parallel offer must never consult sbx: %s", b)
	}
}

// TestSetupCredentials_NonInteractiveNeverPromptsForParallel: a script (no
// TTY) must never see the Parallel offer prompt, and secrets.env must stay
// untouched — the noninteractive no-prompt invariant applies here exactly
// as it does to the 1Password model-key offer.
func TestSetupCredentials_NonInteractiveNeverPromptsForParallel(t *testing.T) {
	home, _ := setupHome(t)
	var out, errb bytes.Buffer
	// A reader that would answer "yes" if ever read from — proving the offer
	// never even asks on a non-interactive run.
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader("y\nop://v/parallel/key\n"), Interactive: false})

	if strings.Contains(out.String(), "Configure Parallel web search now") {
		t.Errorf("non-interactive setup must never show the Parallel prompt:\n%s", out.String())
	}
	body, err := os.ReadFile(filepath.Join(home, "secrets.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "PARALLEL_API_KEY=op://v/parallel/key") {
		t.Errorf("non-interactive setup must never write a ref it never asked for:\n%s", body)
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

// TestSetupCredentials_NormalStatusHasNoPixSetupPrefix proves the tightened
// copy rule: normal, non-error status lines ("no model provider key is
// configured yet", the Parallel web-search state) drop the `pix setup:`
// prefix entirely. Only an actual failure (the secrets-file create error)
// keeps it.
func TestSetupCredentials_NormalStatusHasNoPixSetupPrefix(t *testing.T) {
	_, _ = setupHome(t)
	var out, errb bytes.Buffer
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false})

	if strings.Contains(out.String(), "pix setup:") {
		t.Errorf("normal setup status must not carry the pix setup: prefix:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no model provider key is configured yet") {
		t.Errorf("want the unprefixed status line, got:\n%s", out.String())
	}
}

// TestSetupCredentials_NeverNarratesSecretsFileCreationOrPresence proves the
// tightened copy: setup establishes secrets.env silently, on both a fresh
// home (created) and a rerun (already present) — no "created ..." or
// "secrets file present at ..." line either way. Only an actual failure to
// create it still speaks, and keeps the `pix setup:` prefix.
func TestSetupCredentials_NeverNarratesSecretsFileCreationOrPresence(t *testing.T) {
	home, _ := setupHome(t)
	var out, errb bytes.Buffer
	// First run: secrets.env does not exist yet, so this call creates it.
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false})
	for _, forbidden := range []string{"created " + filepath.Join(home, "secrets.env"), "secrets file present"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("setup must not narrate secrets.env creation, found %q:\n%s", forbidden, out.String())
		}
	}

	// Second run: secrets.env already exists, so this call finds it present.
	out.Reset()
	setupCredentials(&cli.Deps{Sys: sys.Real{}, Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false})
	for _, forbidden := range []string{"created " + filepath.Join(home, "secrets.env"), "secrets file present"} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("setup must not narrate secrets.env presence on a rerun, found %q:\n%s", forbidden, out.String())
		}
	}
}
