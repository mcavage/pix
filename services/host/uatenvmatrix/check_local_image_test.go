// check_local_image_test.go proves environment_uses_local_candidate_image
// (AC-2, docs/design/environments.md section 11) in isolation, against an
// injected fake Executor and this check's own function — the same posture
// isolation_test.go uses for buildExecArgv, so this unit's tests never need
// to know about, or update, the first named check's Run()-level tests.
//
// Host UAT run run-20260824-091306-29559f3a found this check invalid on two
// counts before it ever proved anything: its fixture declared `agent: pix`
// with no kit, so a real `sbx env create` refused outright with `"pix" is
// not a known agent` before create was ever attempted; and the check's own
// digest comparison expected a fabricated `image digest: sha256:...` line
// in the create log that a real `sbx env create` never prints. This file's
// tests are rewritten against the corrected contract: the fixture declares
// `kits: [./kit]` exactly like every other `agent: pix` fixture in this
// package (materialized through writeAuthoredFixture), and the actual
// created sandbox's image identity is probed independently through
// `docker inspect --format {{.Image}} <exact sandbox name>` — a second,
// distinct Executor call after the create receipt — rather than parsed out
// of the create log.
package uatenvmatrix

import (
	"context"
	"strings"
	"testing"
)

// localImageFakeExecutor answers `docker image inspect` (the local
// candidate image lookup), `docker inspect` (the actual created sandbox's
// image identity lookup), and `sbx env create`/cleanup calls
// deterministically — exactly the seam AC-2 requires: no real sbx or docker
// binary is ever invoked under go test. The two `docker` calls are
// distinguished by their own argv shape (image inspect always starts with
// "image"; the actual-container probe never does), never by call order,
// since a caller could reorder calls without this fake silently answering
// the wrong one.
type localImageFakeExecutor struct {
	localImageID    string
	localImageErr   error
	actualImageID   string
	actualImageErr  error
	createOut       string
	createErr       error
	cleanupProbeOut string
	cleanupRmErr    error
}

func (f localImageFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	if name == "docker" {
		if len(args) > 0 && args[0] == "image" {
			return f.localImageID, "", f.localImageErr
		}
		return f.actualImageID, "", f.actualImageErr
	}
	if len(args) > 0 && args[0] == "ls" {
		probeOut := f.cleanupProbeOut
		if probeOut == "" {
			probeOut = f.createOut
		}
		return probeOut, "", nil
	}
	if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
		return "removed\n", "", f.cleanupRmErr
	}
	return f.createOut, "", f.createErr
}

const fakeImageIDA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const fakeImageIDB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestCheckEnvironmentUsesLocalCandidateImage_DigestMismatchFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		localImageID:  fakeImageIDA + "\n",
		createOut:     "created " + candidateImageFixtureName + " (positively identified)\n",
		actualImageID: fakeImageIDB + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err == nil {
		t.Fatal("expected an image-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), fakeImageIDA) || !strings.Contains(err.Error(), fakeImageIDB) {
		t.Fatalf("error does not name both the expected and actual image IDs: %v", err)
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_RegistryPullObservedFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		localImageID:  fakeImageIDA + "\n",
		createOut:     "Pulling from docker.io/mcavage/pix\nDownload complete\ncreated " + candidateImageFixtureName + " (positively identified)\n",
		actualImageID: fakeImageIDA + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err == nil {
		t.Fatal("expected a registry-pull error, got nil")
	}
	if !strings.Contains(err.Error(), "registry pull") {
		t.Fatalf("error does not name the registry pull: %v", err)
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_NeutralImageTextNotClassifiedAsPull
// proves the negative scan does not misclassify neutral create-log text a
// real `sbx env create` is known to print around an already-present local
// image (e.g. "PREPARE IMAGE", "already present") as a registry pull.
func TestCheckEnvironmentUsesLocalCandidateImage_NeutralImageTextNotClassifiedAsPull(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		localImageID:  fakeImageIDA + "\n",
		createOut:     "PREPARE IMAGE\nimage already present locally\ncreated " + candidateImageFixtureName + " (positively identified)\n",
		actualImageID: fakeImageIDA + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err != nil {
		t.Fatalf("neutral create-log text must not be classified as a registry pull, got: %v", err)
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_EqualDigestNoPullPasses(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		localImageID:  fakeImageIDA + "\n",
		createOut:     "created " + candidateImageFixtureName + " (positively identified)\n",
		actualImageID: fakeImageIDA + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err != nil {
		t.Fatalf("expected success for equal image IDs with no pull, got: %v", err)
	}
	if !strings.Contains(lw.String(), fakeImageIDA) {
		t.Errorf("log does not record the matched image ID: %s", lw.String())
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_NoImageTagFailsClosed(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{localImageID: fakeImageIDA, createOut: "irrelevant"}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "")
	if err == nil {
		t.Fatal("expected an error when no candidate image tag is supplied")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_MissingActualImageIDFailsClosed
// proves the check fails closed (never silently skips the comparison) when
// the actual-container probe reports no image ID at all.
func TestCheckEnvironmentUsesLocalCandidateImage_MissingActualImageIDFailsClosed(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		localImageID:  fakeImageIDA + "\n",
		createOut:     "created " + candidateImageFixtureName + " (positively identified)\n",
		actualImageID: "",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err == nil {
		t.Fatal("expected an error when the actual-container probe never reports an image ID")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_ActualImageInspectCommandShape
// pins the exact argv this check issues to inspect the actually created
// sandbox's image identity: `docker inspect --format {{.Image}} <exact
// sandbox name>` — the narrowest, established observable this repo can
// support (sandbox/list.go's own `container_name` key alias documents that
// a sandbox's docker container is addressable by the sandbox's own name).
func TestCheckEnvironmentUsesLocalCandidateImage_ActualImageInspectCommandShape(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	var dockerCalls [][]string
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "image" {
			return fakeImageIDA + "\n", "", nil
		}
		if len(args) > 0 && args[0] == "inspect" {
			dockerCalls = append(dockerCalls, append([]string(nil), args...))
			return fakeImageIDA + "\n", "", nil
		}
		if len(args) > 0 && args[0] == "ls" {
			return "created " + candidateImageFixtureName + " (positively identified)\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			return "removed\n", "", nil
		}
		return "created " + candidateImageFixtureName + " (positively identified)\n", "", nil
	}}
	if err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test"); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(dockerCalls) != 1 {
		t.Fatalf("expected exactly 1 actual-image `docker inspect` call, got %d: %#v", len(dockerCalls), dockerCalls)
	}
	want := []string{"inspect", "--format", "{{.Image}}", candidateImageFixtureName}
	if len(dockerCalls[0]) != len(want) {
		t.Fatalf("docker inspect call = %#v, want %#v", dockerCalls[0], want)
	}
	for i, w := range want {
		if dockerCalls[0][i] != w {
			t.Errorf("docker inspect call args[%d] = %q, want %q (full: %#v)", i, dockerCalls[0][i], w, dockerCalls[0])
		}
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_CleanupRunsAndPrimaryErrorWins
// proves receipt-gated cleanup still runs (a fresh `sbx ls --json` probe,
// then `sbx env rm -f`) even when the check itself fails on an image-ID
// mismatch, and that the mismatch error — not cleanup's own nil result —
// is what Run() ultimately reports (primary-error precedence).
func TestCheckEnvironmentUsesLocalCandidateImage_CleanupRunsAndPrimaryErrorWins(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	var sawLsProbe, sawRemove bool
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "image" {
			return fakeImageIDA + "\n", "", nil
		}
		if len(args) > 0 && args[0] == "ls" {
			sawLsProbe = true
			return "created " + candidateImageFixtureName + " (positively identified)\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			sawRemove = true
			return "removed\n", "", nil
		}
		if len(args) > 0 && args[0] == "inspect" {
			// The actual-container probe reports a DIFFERENT image than the
			// local candidate, forcing the check's own primary failure.
			return fakeImageIDB + "\n", "", nil
		}
		return "created " + candidateImageFixtureName + " (positively identified)\n", "", nil
	}}

	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err == nil {
		t.Fatal("expected the image-mismatch error to propagate")
	}
	if !strings.Contains(err.Error(), fakeImageIDB) {
		t.Fatalf("Run's reported error must be the primary image-mismatch error, got: %v", err)
	}
	if !sawLsProbe {
		t.Error("expected cleanup's fresh `sbx ls --json` probe to run despite the check's own failure")
	}
	if !sawRemove {
		t.Error("expected cleanup's `sbx env rm -f` to run despite the check's own failure")
	}
}

func TestCheckNames_IncludesLocalCandidateImageCheckInDeterministicOrder(t *testing.T) {
	names := CheckNames()
	want := []string{
		"environment_create_then_exec_invocation",
		"environment_uses_local_candidate_image",
		"environment_recreate_boundary",
		"environment_failed_create_cleanup",
		"environment_rm_scope_refusal",
		"environment_custom_agent_ollama",
	}
	if len(names) != len(want) {
		t.Fatalf("CheckNames() = %#v, want %#v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("CheckNames()[%d] = %q, want %q", i, names[i], want[i])
		}
	}
}
