// setup_default_offer_test.go — the reported product gap: `pix setup --env
// work` finished successfully and a bare `pix` still launched `default`,
// because nothing ever moved the machine default. The environment was
// perfectly set up and completely unused, with no visible reason.
//
// The fix is an OFFER, not an assumption: an interactive default-Yes
// question at the end of a successful named setup, no write at all on a
// non-interactive terminal, and the same single writer `pix env default
// NAME` owns.
package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/pixhome"
	"pix/host/workflow/provision"
)

// defaultOfferHome is a PIX_HOME whose recorded machine default is
// `default` — the state provision.EnsureDefaultEnvironment leaves behind,
// and exactly the state the user hit.
func defaultOfferHome(t *testing.T) pixhome.Paths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := pixhome.New(home)
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := config.SetDefaultEnvironmentAt(home, provision.DefaultEnvironmentName); err != nil {
		t.Fatalf("seed the machine default: %v", err)
	}
	return p
}

func recordedDefault(t *testing.T, home string) string {
	t.Helper()
	cfg, err := config.LoadFrom(config.PathAt(home))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg.DefaultEnvironment
}

func TestOfferDefaultEnvironment_AcceptedSelectsIt(t *testing.T) {
	p := defaultOfferHome(t)
	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("\n"), Interactive: true}

	offerDefaultEnvironment(d, p, "work")

	if !strings.Contains(out.String(), "Use work as the default environment") {
		t.Fatalf("a successful named setup must offer the default; got:\n%s", out.String())
	}
	if got := recordedDefault(t, p.Home); got != "work" {
		t.Fatalf("default_environment = %q, want %q (an empty answer takes the [Y/n] default)", got, "work")
	}
	if !strings.Contains(out.String(), "Default environment: work.") {
		t.Fatalf("the outcome must be stated, not assumed; got:\n%s", out.String())
	}
}

func TestOfferDefaultEnvironment_DeclinedChangesNothing(t *testing.T) {
	p := defaultOfferHome(t)
	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("n\n"), Interactive: true}

	offerDefaultEnvironment(d, p, "work")

	if got := recordedDefault(t, p.Home); got != provision.DefaultEnvironmentName {
		t.Fatalf("default_environment = %q, want it untouched at %q", got, provision.DefaultEnvironmentName)
	}
	if !strings.Contains(out.String(), "pix run --env work") {
		t.Fatalf("declining must name how to launch it explicitly; got:\n%s", out.String())
	}
}

// A script that ran `pix setup --env NAME` never asked for this host's
// machine default to move (invariant 3: non-interactive stdin never mutates
// host state as a side effect). It gets the command instead.
func TestOfferDefaultEnvironment_NonInteractiveNeverWrites(t *testing.T) {
	p := defaultOfferHome(t)
	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, Interactive: false}

	offerDefaultEnvironment(d, p, "work")

	if got := recordedDefault(t, p.Home); got != provision.DefaultEnvironmentName {
		t.Fatalf("default_environment = %q, want it untouched at %q", got, provision.DefaultEnvironmentName)
	}
	if !strings.Contains(out.String(), "pix env default work") {
		t.Fatalf("a non-interactive run must name the command instead of asking; got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Use work as the default environment") {
		t.Fatalf("a non-interactive run must not print a question nobody can answer; got:\n%s", out.String())
	}
}

// Already selected: nothing to ask, nothing to write, and the terminal says
// so rather than re-asking every setup rerun.
func TestOfferDefaultEnvironment_AlreadyDefaultAsksNothing(t *testing.T) {
	p := defaultOfferHome(t)
	if err := config.SetDefaultEnvironmentAt(p.Home, "work"); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	stdin := &countingReader{s: "n\n"}
	d := &cli.Deps{Out: &out, Err: &errb, In: stdin, Interactive: true}

	offerDefaultEnvironment(d, p, "work")

	if stdin.reads > 0 {
		t.Fatalf("an already-default environment must not read stdin (%d reads)", stdin.reads)
	}
	if !strings.Contains(out.String(), "already the default") {
		t.Fatalf("got:\n%s", out.String())
	}
}

// The CALLER test: the offer is wired into `pix setup --env NAME`'s own
// success path (invariant: a feature is not done until its caller is
// wired), and only there — a bare `pix setup` never touches the default
// this way.
func TestSetupEnv_SuccessOffersTheDefault(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p := writeSetupEnvFixture(t, home, "work", "schema = 1\n")
	preTrustSetupEnv(t, p, "work")
	if err := config.SetDefaultEnvironmentAt(home, provision.DefaultEnvironmentName); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("y\n"), Interactive: true}
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("setupSelectedEnvironment: %v\n%s%s", err, out.String(), errb.String())
	}
	offerDefaultEnvironment(d, p, "work")

	if got := recordedDefault(t, home); got != "work" {
		t.Fatalf("default_environment = %q, want %q after an accepted offer", got, "work")
	}
}
