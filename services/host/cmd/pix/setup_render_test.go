// setup_render_test.go proves `pix setup`'s reported UX: a quiet, two-line
// normal report (PIX_HOME line + one concise verified outcome line), full
// per-artifact narration gated behind --verbose, and — when readiness
// fails — the actual container/MCP reason and exact remedy printed
// unconditionally, never a bare "run pix doctor" deflection.
package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

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
// report is exactly two substantive lines: the PIX_HOME line and one
// concise verified outcome line. Every per-artifact narration (runtime,
// default env, container action, MCP registration) is suppressed.
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
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("quiet successful setup must not narrate artifacts, found %q:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "PIX_HOME") {
		t.Errorf("want the PIX_HOME line, got:\n%s", got)
	}
	if !strings.Contains(got, "pix setup: ready.") {
		t.Errorf("want the one concise verified outcome line, got:\n%s", got)
	}
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("want exactly 2 lines on a quiet successful run, got %d:\n%s", len(lines), got)
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
		"pix setup: ready.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verbose setup is missing %q:\n%s", want, got)
		}
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
	if len(lines) != 2 {
		t.Errorf("want exactly 2 lines on a quiet no-op rerun, got %d:\n%s", len(lines), got)
	}
	if !strings.Contains(got, "pix setup: ready.") {
		t.Errorf("a converged rerun must still report ready:\n%s", got)
	}
}
