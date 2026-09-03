package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/cli"
	"pix/host/pixhome"
	nativeenv "pix/host/workflow/env"
)

// setup_hooks_test.go wires the `[[setup]]` hook feature to its REAL
// caller: `pix setup --env NAME` (setupSelectedEnvironment). The runner's
// own semantics are proven at process level in envsetup/hooks_test.go; what
// is proven here is that the command actually reaches it, only after trust,
// and refuses when it should.

// hookEnvFixture writes an environment with one `[[setup]]` hook whose
// executable is scriptBody, and returns the PIX_HOME paths plus the marker
// path the hook's apply step touches.
func hookEnvFixture(t *testing.T, home, name, sidecar, scriptBody string) (pixhome.Paths, string) {
	t.Helper()
	p := writeSetupEnvFixture(t, home, name, sidecar)
	root := p.EnvironmentDir(name)
	marker := filepath.Join(root, "applied")
	body := strings.ReplaceAll(scriptBody, "@MARKER@", marker)
	if err := os.WriteFile(filepath.Join(root, "setup-tool"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p, marker
}

const hookSidecar = `schema = 1

[[setup]]
id = "tool"
command = "./setup-tool"
check_args = ["check"]
apply_args = ["install"]
required = true
kind = "install"
`

func TestSetupEnv_RunsSetupHook_CheckApplyCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, marker := hookEnvFixture(t, home, "work", hookSidecar, `#!/bin/sh
case "$1" in
  check) [ -f "@MARKER@" ] && exit 0 || exit 1 ;;
  install) touch "@MARKER@"; exit 0 ;;
esac
exit 2
`)
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("setupSelectedEnvironment: %v\n%s%s", err, out.String(), errb.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the hook's apply step never ran: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "post-check passed") {
		t.Fatalf("setup must earn its success word from the post-check:\n%s", out.String())
	}

	// Rerun: converged, so nothing is applied again and setup still passes.
	out.Reset()
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("rerun: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "already ready") {
		t.Fatalf("a rerun on a converged host must report already-ready, got:\n%s", out.String())
	}
}

func TestSetupEnv_RequiredHookNeverBecomesReady_FailsSetup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, _ := hookEnvFixture(t, home, "work", hookSidecar, `#!/bin/sh
case "$1" in
  check) exit 1 ;;
  install) exit 0 ;;
esac
exit 2
`)
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	err := setupSelectedEnvironment(d, p, "work")
	if err == nil {
		t.Fatalf("a required hook that is not ready must fail setup:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("the failure must be honest about the state, got %q", err)
	}
}

// TestSetupEnv_MultipleHooksRunInAuthoredOrder proves the regression a
// live `pix setup --env work` run surfaced: gworkspace-style dependent
// hooks executed before a hook declared ahead of it in pix.toml, because
// something between the sidecar and the runner was re-sorting the
// `[[setup]]` list by id. This declares two hooks whose ids are the exact
// INVERSE of authored order ("zeta-first" before "alpha-second"): an
// alphabetical sort would flip them, so any regression here is unambiguous.
// Both the actual check/apply execution order (order.log, written by each
// hook's own apply step) and the printed narration order must match
// authoring, never id order.
func TestSetupEnv_MultipleHooksRunInAuthoredOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	const sidecar = `schema = 1

[[setup]]
id = "zeta-first"
command = "./setup-tool"
check_args = ["check-zeta"]
apply_args = ["install-zeta"]
required = true
kind = "install"

[[setup]]
id = "alpha-second"
command = "./setup-tool"
check_args = ["check-alpha"]
apply_args = ["install-alpha"]
required = true
kind = "install"
`
	p := writeSetupEnvFixture(t, home, "work", sidecar)
	root := p.EnvironmentDir("work")
	orderLog := filepath.Join(root, "order.log")
	readyZeta := filepath.Join(root, "ready-zeta")
	readyAlpha := filepath.Join(root, "ready-alpha")
	script := `#!/bin/sh
case "$1" in
  check-zeta) [ -f "` + readyZeta + `" ] && exit 0 || exit 1 ;;
  install-zeta) echo zeta-first >> "` + orderLog + `"; touch "` + readyZeta + `"; exit 0 ;;
  check-alpha) [ -f "` + readyAlpha + `" ] && exit 0 || exit 1 ;;
  install-alpha) echo alpha-second >> "` + orderLog + `"; touch "` + readyAlpha + `"; exit 0 ;;
esac
exit 2
`
	if err := os.WriteFile(filepath.Join(root, "setup-tool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("setupSelectedEnvironment: %v\n%s%s", err, out.String(), errb.String())
	}

	logged, err := os.ReadFile(orderLog)
	if err != nil {
		t.Fatalf("order.log was never written: %v", err)
	}
	if got, want := string(logged), "zeta-first\nalpha-second\n"; got != want {
		t.Fatalf("hooks executed out of authored order: got %q, want %q (an id-alphabetical sort would produce the reverse)", got, want)
	}

	zetaAt := strings.Index(out.String(), "zeta-first (install)")
	alphaAt := strings.Index(out.String(), "alpha-second (install)")
	if zetaAt == -1 || alphaAt == -1 {
		t.Fatalf("expected both hooks narrated by id, got:\n%s", out.String())
	}
	if zetaAt > alphaAt {
		t.Fatalf("setup narrated alpha-second before zeta-first, must preserve authored order:\n%s", out.String())
	}

	// The verbose trust bill itemizes hooks in the SAME field order (safety
	// invariant: authored order everywhere, never id order).
	sel, err := nativeenv.ResolveIn(p, "work")
	if err != nil {
		t.Fatal(err)
	}
	bom, _, err := environmentBoM(sel)
	if err != nil {
		t.Fatal(err)
	}
	if len(bom.SetupHooks) != 2 || bom.SetupHooks[0].ID != "zeta-first" || bom.SetupHooks[1].ID != "alpha-second" {
		t.Fatalf("bill of materials must preserve authored [[setup]] order, got %+v", bom.SetupHooks)
	}
	var bill bytes.Buffer
	renderTrustBill(&bill, "work", bom, true)
	bs := bill.String()
	zetaBillAt := strings.Index(bs, "setup hook:        zeta-first")
	alphaBillAt := strings.Index(bs, "setup hook:        alpha-second")
	if zetaBillAt == -1 || alphaBillAt == -1 || zetaBillAt > alphaBillAt {
		t.Fatalf("verbose trust bill must list setup hooks in authored order, got:\n%s", bs)
	}
}

// The hook runs ONLY after trust. A non-interactive run of an UNTRUSTED
// environment fails closed at the review, having executed nothing.
func TestSetupEnv_UntrustedNonInteractive_FailsClosedBeforeAnyHookRuns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, marker := hookEnvFixture(t, home, "work", hookSidecar, `#!/bin/sh
touch "@MARKER@"
exit 0
`)
	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false}
	if err := setupSelectedEnvironment(d, p, "work"); err == nil {
		t.Fatal("an untrusted environment must refuse on a non-interactive terminal")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook ran before the environment was trusted")
	}
}

// A hook swapped between the accepted review and the run is refused by the
// command, not just by the runner package. There are TWO independent
// refusals in that path and this proves the pair holds end to end: the
// content hash is part of the accepted fingerprint (so trust no longer
// matches and the review re-fires, refusing here because there is no
// terminal to answer it), and — for anything that gets past that — the
// runner re-proves the executable immediately before exec
// (envsetup.TestRun_ExecutableMutatedAfterReview_RefusesBeforeExecuting).
// Either way the swapped code never runs.
func TestSetupEnv_HookExecutableSwappedAfterTrust_Refuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, marker := hookEnvFixture(t, home, "work", hookSidecar, `#!/bin/sh
exit 0
`)
	preTrustSetupEnv(t, p, "work")
	tool := filepath.Join(p.EnvironmentDir("work"), "setup-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\ntouch \""+marker+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false}
	if err := setupSelectedEnvironment(d, p, "work"); err == nil {
		t.Fatal("a hook whose executable changed after acceptance must refuse")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the swapped executable RAN")
	}
	// The re-fired review shows the setup hook's CURRENT bill (the fresh
	// fingerprint from what is on disk now, not what they accepted before),
	// so a human is asked about the new content rather than the old one.
	if !strings.Contains(out.String(), "1 setup hook(s)") {
		t.Fatalf("the re-fired review must show the setup hook again:\n%s%s", out.String(), errb.String())
	}
}

const hookSidecarWithInput = `schema = 1

[[setup]]
id = "tool"
command = "./setup-tool"
check_args = ["check"]
apply_args = ["install"]
required = true
kind = "install"
inputs = ["lib/helper.sh"]
`

// A DECLARED input is copied into the hook's execution snapshot at its
// declared relative path, so a hook that reads it relative to its own cwd
// (the snapshot root) finds it there.
func TestSetupEnv_DeclaredInputIsUsableFromTheSnapshotCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, marker := hookEnvFixture(t, home, "work", hookSidecarWithInput, `#!/bin/sh
case "$1" in
  check) [ -f "@MARKER@" ] && exit 0 || exit 1 ;;
  install)
    [ -f lib/helper.sh ] || exit 3
    grep -q COMPANION lib/helper.sh || exit 4
    touch "@MARKER@"
    exit 0
    ;;
esac
exit 2
`)
	root := p.EnvironmentDir("work")
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "helper.sh"), []byte("COMPANION\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	if err := setupSelectedEnvironment(d, p, "work"); err != nil {
		t.Fatalf("setupSelectedEnvironment: %v\n%s%s", err, out.String(), errb.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the hook never read its declared input from the snapshot cwd: %v\n%s", err, out.String())
	}
}

// An UNDECLARED sibling — a file that lives next to setup-tool in the
// environment directory but was never named in `inputs` — is NOT present
// in the hook's execution snapshot: the snapshot's cwd contains only the
// reviewed executable and its declared inputs.
func TestSetupEnv_UndeclaredSiblingIsUnavailableInTheSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, _ := hookEnvFixture(t, home, "work", hookSidecar, `#!/bin/sh
case "$1" in
  check) exit 1 ;;
  install) [ -f sibling.txt ] && exit 0 || exit 5 ;;
esac
exit 2
`)
	root := p.EnvironmentDir("work")
	// sibling.txt sits right next to setup-tool on disk, but hookSidecar
	// declares no `inputs` at all.
	if err := os.WriteFile(filepath.Join(root, "sibling.txt"), []byte("undeclared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preTrustSetupEnv(t, p, "work")

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb}
	err := setupSelectedEnvironment(d, p, "work")
	if err == nil {
		t.Fatal("a hook that depends on an undeclared sibling must not become ready")
	}
	if !strings.Contains(out.String(), "exited 5") && !strings.Contains(err.Error(), "exited 5") {
		t.Fatalf("want the install step to fail with exit 5 (sibling.txt absent from the snapshot cwd), got err=%v out=%s", err, out.String())
	}
}

// A companion input mutated after trust re-gates exactly like the
// executable itself: the fingerprint moves, so the pre-computed trust
// record no longer matches and setup refuses before any hook runs.
func TestSetupEnv_InputMutatedAfterTrust_Refuses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, marker := hookEnvFixture(t, home, "work", hookSidecarWithInput, `#!/bin/sh
case "$1" in
  check) [ -f "@MARKER@" ] && exit 0 || exit 1 ;;
  install) cat lib/helper.sh > "@MARKER@"; exit 0 ;;
esac
exit 2
`)
	root := p.EnvironmentDir("work")
	if err := os.MkdirAll(filepath.Join(root, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "lib", "helper.sh"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	preTrustSetupEnv(t, p, "work")

	// Mutate the companion AFTER the trust record was written for the
	// reviewed content.
	if err := os.WriteFile(filepath.Join(root, "lib", "helper.sh"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader(""), Interactive: false}
	if err := setupSelectedEnvironment(d, p, "work"); err == nil {
		t.Fatal("a mutated declared input must refuse setup, not silently run with new content")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the hook ran despite the mutated, unreviewed companion input")
	}
}

// The default (non-verbose) consent screen is a SUMMARY: it counts the
// setup hook but names neither its id nor its argv — that detail, like
// every other section of this bill, is exactly what --verbose restores.
func TestRenderTrustBill_SummarizesSetupHooksByDefaultShowsDetailVerbose(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIX_HOME", home)
	p, _ := hookEnvFixture(t, home, "work", hookSidecar, "#!/bin/sh\nexit 0\n")
	sel, err := nativeenv.ResolveIn(p, "work")
	if err != nil {
		t.Fatal(err)
	}
	bom, _, err := environmentBoM(sel)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	renderTrustBill(&out, "work", bom, false)
	s := out.String()
	if !strings.Contains(s, "1 setup hook(s)") {
		t.Errorf("the default consent screen omits the setup hook count:\n%s", s)
	}
	if !strings.Contains(s, "env trust work --verbose") {
		t.Errorf("the default consent screen should point at --verbose for detail:\n%s", s)
	}
	// Every per-hook fact — id, kind, required/optional, argv, digests — is
	// review DETAIL, gated behind --verbose exactly like every other section
	// in this bill: the default screen is counts/risk categories only.
	for _, notWant := range []string{
		"setup hook:        tool",
		"check: ./setup-tool check",
		"apply: ./setup-tool install",
		"sha256:",
	} {
		if strings.Contains(s, notWant) {
			t.Errorf("the default consent screen should not print hook detail %q behind no --verbose:\n%s", notWant, s)
		}
	}

	var verboseOut bytes.Buffer
	renderTrustBill(&verboseOut, "work", bom, true)
	vs := verboseOut.String()
	for _, want := range []string{
		"setup hook:        tool (install, required) ./setup-tool",
		"check: ./setup-tool check",
		"apply: ./setup-tool install",
		"sha256:",
	} {
		if !strings.Contains(vs, want) {
			t.Errorf("--verbose omits %q:\n%s", want, vs)
		}
	}
}
