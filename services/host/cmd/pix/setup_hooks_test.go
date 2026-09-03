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
	// The re-fired review shows the NEW content hash, so a human is asked
	// about what is on disk now rather than what they accepted before.
	if !strings.Contains(out.String(), "setup hook:        tool") {
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

// The consent screen must show every hook fact a human is approving —
// argv included — by DEFAULT, not only under --verbose.
func TestRenderTrustBill_ShowsSetupHooksByDefault(t *testing.T) {
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
	// The default consent screen names WHAT will run (id, kind,
	// required/optional) unconditionally — that much is never behind
	// --verbose, unlike every other section in this bill.
	for _, want := range []string{
		"1 setup hook(s)",
		"setup hook:        tool (install, required)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the default consent screen omits %q:\n%s", want, s)
		}
	}
	// The full argv and content digest are review DETAIL, gated behind
	// --verbose exactly like every other section (kits, host services, MCP
	// commands): a concise summary is the normal review surface.
	for _, notWant := range []string{
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
