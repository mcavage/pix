// setup_render_test.go proves `pix setup`'s reported UX: a quiet, one-line
// normal report on success (just the PIX_HOME line — no separate "ready"
// narration, since a zero exit already says that), full per-artifact
// narration gated behind --verbose, and — when readiness fails — the actual
// container/MCP reason and exact remedy printed unconditionally, never a
// bare "run pix doctor" deflection.
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/workflow/provision"
)

func readyResult() provision.Result {
	return provision.Result{
		ReleaseInstalled: true,
		Runtime:          release.RuntimeInstall{Extracted: true, Dir: "/home/.pix/runtime/2.0.0"},
		KitRevision:      "0123456789abcdef0123456789abcdef01234567",
		Container:        container.Result{Action: container.ActionCreated, ID: "abc123"},
		MCPRegistered:    true,
		MCPState:         provision.MCPRegistrationAdded,
		MCPName:          "pix-memory-0123456789abcdef",
	}
}

// TestRenderSetupResult_QuietOnSuccess proves the default (non-verbose)
// report is exactly one substantive line: the PIX_HOME line. There is no
// separate "ready"/"For the full host report" line, and every per-artifact
// narration (runtime, default env, container action, MCP registration) is
// suppressed.
func TestRenderSetupResult_QuietOnSuccess(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	res := readyResult()

	renderSetupResult(d, pixhome.New(t.TempDir()), res, false)

	got := out.String()
	for _, forbidden := range []string{
		"runtime installed",
		"pix-memory container:",
		"MCP registration:",
		"pix setup: ready",
		"For the full host report",
		"pix setup:",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("quiet successful setup must not narrate artifacts or say ready, found %q:\n%s", forbidden, got)
		}
	}
	if !strings.HasPrefix(got, "PIX_HOME already initialized") {
		t.Errorf("want the unprefixed PIX_HOME line, got:\n%s", got)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("want exactly 1 line on a quiet successful run, got %d:\n%s", len(lines), got)
	}
}

// TestRenderSetupResult_VerboseShowsArtifacts proves --verbose restores the
// full per-artifact narration this UX otherwise suppresses.
func TestRenderSetupResult_VerboseShowsArtifacts(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	res := readyResult()

	renderSetupResult(d, pixhome.New(t.TempDir()), res, true)

	got := out.String()
	for _, want := range []string{
		"runtime installed",
		"pix-memory container: created",
		"MCP registration: registered",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verbose setup is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pix setup: ready") {
		t.Errorf("a successful run must not narrate a separate ready line, even under --verbose:\n%s", got)
	}
}

// TestRenderSetupResult_ProbeFailureNamesReason proves a failed readiness
// probe is reported with the ACTUAL probe error and an exact next action —
// not a generic "run pix doctor" deflection — and this reason is printed
// even without --verbose.
func TestRenderSetupResult_ProbeFailureNamesReason(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	res := readyResult()
	res.Container.ProbeErr = errors.New("connection refused")

	renderSetupResult(d, pixhome.New(t.TempDir()), res, false)

	got := out.String()
	if res.Ready() {
		t.Fatal("a probe error must never report ready")
	}
	for _, want := range []string{
		"pix setup: not ready.",
		"connection refused",
		"docker logs abc123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("probe failure is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "pix setup: ready") {
		t.Errorf("must not claim ready:\n%s", got)
	}
}

// TestRenderSetupResult_RefusedReplaceNamesReason proves a declined
// container replace is reported with the actual drift and the exact manual
// remedy, not a bare doctor deflection.
func TestRenderSetupResult_RefusedReplaceNamesReason(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	res := provision.Result{
		Container: container.Result{
			Action:              container.ActionRefusedReplace,
			ID:                  "old123",
			PreviousImage:       "pix-memory:old",
			PreviousFingerprint: "fp-old",
		},
	}

	renderSetupResult(d, pixhome.New(t.TempDir()), res, false)

	got := out.String()
	for _, want := range []string{
		"pix setup: not ready.",
		"declined",
		"pix-memory:old",
		"docker rm -f old123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refused-replace report is missing %q:\n%s", want, got)
		}
	}
}

// TestRenderSetupResult_ForeignStackNamesReason proves a name collision with
// a different Pix stack is reported with the actual owning stack id and an
// exact remedy, not a bare doctor deflection.
func TestRenderSetupResult_ForeignStackNamesReason(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	res := provision.Result{
		Container: container.Result{
			Action:         container.ActionRefusedForeignStack,
			ID:             "foreign123",
			ForeignStackID: "otherstack0000",
		},
	}

	renderSetupResult(d, pixhome.New(t.TempDir()), res, false)

	got := out.String()
	for _, want := range []string{
		"pix setup: not ready.",
		"otherstack0000",
		"docker rm -f foreign123",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("foreign-stack report is missing %q:\n%s", want, got)
		}
	}
}

// TestRenderSetupResult_QuietOnRerunNoOp proves an idempotent rerun where
// nothing changed (no release install, no default env, container merely
// adopted) is exactly as quiet as a fresh success: the "quiet no-op"
// contract this UX must preserve.
func TestRenderSetupResult_QuietOnRerunNoOp(t *testing.T) {
	var out bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &out}
	res := provision.Result{
		Container:     container.Result{Action: container.ActionAdopted, ID: "abc123"},
		MCPRegistered: true,
		MCPState:      provision.MCPRegistrationPresentVerified,
		MCPName:       "pix-memory-0123456789abcdef",
	}

	renderSetupResult(d, pixhome.New(t.TempDir()), res, false)

	got := out.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 1 {
		t.Errorf("want exactly 1 line on a quiet no-op rerun, got %d:\n%s", len(lines), got)
	}
	if !res.Ready() {
		t.Fatal("an adopted container with a verified MCP registration must be Ready")
	}
	if strings.Contains(got, "pix setup: ready") || strings.Contains(got, "For the full host report") {
		t.Errorf("a converged rerun's own zero exit already says ready; no separate line is printed:\n%s", got)
	}
}

// TestConfirmContainerReplace_DefaultSaysOnlyChangedAndPreserved proves the
// normal (non-verbose) memory-replacement prompt names neither the running
// nor wanted image reference nor either fingerprint — it says only that the
// service changed and that /data is preserved either way. --verbose (only)
// restores the exact running/wanted image and fingerprint for anyone who
// wants to confirm precisely what changed, matching `pix env trust`'s own
// counts-by-default/--verbose-for-detail split.
func TestConfirmContainerReplace_DefaultSaysOnlyChangedAndPreserved(t *testing.T) {
	var errb bytes.Buffer
	d := &cli.Deps{
		Out:         &bytes.Buffer{},
		Err:         &errb,
		In:          strings.NewReader("n\n"),
		Interactive: true,
	}
	confirm := confirmContainerReplace(d, false)
	current := container.Info{Image: "pix-memory:old", Labels: map[string]string{container.FingerprintLabel: "fp-old"}}
	want := container.Spec{Image: "pix-memory:new"}

	confirm(current, want)

	got := errb.String()
	for _, wantStr := range []string{"changed", "preserved"} {
		if !strings.Contains(got, wantStr) {
			t.Errorf("default prompt missing %q:\n%s", wantStr, got)
		}
	}
	for _, notWant := range []string{"pix-memory:old", "pix-memory:new", "fp-old"} {
		if strings.Contains(got, notWant) {
			t.Errorf("default prompt should not dump image/fingerprint detail %q:\n%s", notWant, got)
		}
	}
}

// TestConfirmContainerReplace_VerboseShowsImagesAndFingerprints proves
// --verbose restores the exact drift the default prompt above omits.
func TestConfirmContainerReplace_VerboseShowsImagesAndFingerprints(t *testing.T) {
	var errb bytes.Buffer
	d := &cli.Deps{
		Out:         &bytes.Buffer{},
		Err:         &errb,
		In:          strings.NewReader("n\n"),
		Interactive: true,
	}
	confirm := confirmContainerReplace(d, true)
	current := container.Info{Image: "pix-memory:old", Labels: map[string]string{container.FingerprintLabel: "fp-old"}}
	want := container.Spec{Image: "pix-memory:new"}

	confirm(current, want)

	got := errb.String()
	for _, wantStr := range []string{"pix-memory:old", "fp-old", "pix-memory:new"} {
		if !strings.Contains(got, wantStr) {
			t.Errorf("--verbose prompt missing %q:\n%s", wantStr, got)
		}
	}
}

// TestConfirmContainerReplace_BlankEnterDeclinesPromptly is the regression
// for the bug this replaces: a bare Enter (blank answer) must decline
// (default No) and return immediately, never block waiting for a token that
// a blank line never supplies. The prompt used to read with a fresh
// fmt.Fscanln(d.In, &line) instead of the command's one shared line reader,
// which — same failure class LineIn's own doc comment describes — could
// leave a raw read stuck on an input stream a buffered reader elsewhere had
// already drained. Running the call on its own goroutine with a bounded
// timeout is what actually proves "returns promptly", not just "returns the
// right value eventually".
func TestConfirmContainerReplace_BlankEnterDeclinesPromptly(t *testing.T) {
	d := &cli.Deps{
		Out:         &bytes.Buffer{},
		Err:         &bytes.Buffer{},
		In:          strings.NewReader("\n"),
		Interactive: true,
	}
	confirm := confirmContainerReplace(d, false)
	current := container.Info{Image: "pix-memory:old", Labels: map[string]string{container.FingerprintLabel: "fp-old"}}
	want := container.Spec{Image: "pix-memory:new"}

	done := make(chan bool, 1)
	go func() { done <- confirm(current, want) }()
	select {
	case got := <-done:
		if got {
			t.Error("a blank Enter must decline (default No), got true")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("confirmContainerReplace hung on a blank Enter instead of returning the default promptly")
	}
}

// TestConfirmContainerReplace_SharesTheCommandsLineBuffer proves the prompt
// reads through d.Line(), the SAME buffered reader every other prompt in
// this command shares (prompt.go's LineIn doc), rather than a private read
// straight off d.In. A raw per-call reader discards whatever it buffered
// but did not consume when it goes out of scope, so anything typed ahead —
// including the next prompt's own answer — would vanish. Answering "y" here
// and then asking a second question through d.Ask must still see the line
// typed ahead of it.
func TestConfirmContainerReplace_SharesTheCommandsLineBuffer(t *testing.T) {
	d := &cli.Deps{
		Out:         &bytes.Buffer{},
		Err:         &bytes.Buffer{},
		In:          strings.NewReader("y\nafter\n"),
		Interactive: true,
	}
	confirm := confirmContainerReplace(d, false)
	current := container.Info{Image: "pix-memory:old", Labels: map[string]string{container.FingerprintLabel: "fp-old"}}
	want := container.Spec{Image: "pix-memory:new"}

	if !confirm(current, want) {
		t.Fatal("y must confirm the replace")
	}
	got, ok := d.Ask(cli.Question{Label: "NEXT"})
	if !ok || got != "after" {
		t.Fatalf("Ask after confirm = (%q, %v), want the line typed ahead of the confirm prompt still there", got, ok)
	}
}
