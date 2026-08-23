// check_local_image_test.go proves environment_uses_local_candidate_image
// (AC-2, docs/design/environments.md section 11) in isolation, against an
// injected fake Executor and this check's own function — the same posture
// isolation_test.go uses for buildExecArgv, so this unit's tests never need
// to know about, or update, the first named check's Run()-level tests.
package uatenvmatrix

import (
	"context"
	"strings"
	"testing"
)

// localImageFakeExecutor answers "docker image inspect" and "sbx env
// create" deterministically, keyed only on the command name — exactly the
// seam AC-2 requires: no real sbx or docker binary is ever invoked under go
// test.
type localImageFakeExecutor struct {
	inspectDigest string
	inspectErr    error
	createOut     string
	createErr     error
}

func (f localImageFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	if name == "docker" {
		return f.inspectDigest, "", f.inspectErr
	}
	return f.createOut, "", f.createErr
}

const fakeDigestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const fakeDigestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestCheckEnvironmentUsesLocalCandidateImage_DigestMismatchFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		inspectDigest: fakeDigestA + "\n",
		createOut:     "created pix-uatenv-fixture-image (positively identified) image digest: " + fakeDigestB + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err == nil {
		t.Fatal("expected a digest-mismatch error, got nil")
	}
	if !strings.Contains(err.Error(), fakeDigestA) || !strings.Contains(err.Error(), fakeDigestB) {
		t.Fatalf("error does not name both the expected and actual digest: %v", err)
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_RegistryPullObservedFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		inspectDigest: fakeDigestA + "\n",
		createOut:     "Pulling from docker.io/mcavage/pix\nDownload complete\ncreated pix-uatenv-fixture-image (positively identified) image digest: " + fakeDigestA + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err == nil {
		t.Fatal("expected a registry-pull error, got nil")
	}
	if !strings.Contains(err.Error(), "registry pull") {
		t.Fatalf("error does not name the registry pull: %v", err)
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_EqualDigestNoPullPasses(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		inspectDigest: fakeDigestA + "\n",
		createOut:     "created pix-uatenv-fixture-image (positively identified) image digest: " + fakeDigestA + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err != nil {
		t.Fatalf("expected success for equal digests with no pull, got: %v", err)
	}
	if !strings.Contains(lw.String(), fakeDigestA) {
		t.Errorf("log does not record the matched digest: %s", lw.String())
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_NoImageTagFailsClosed(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{inspectDigest: fakeDigestA, createOut: "irrelevant"}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "")
	if err == nil {
		t.Fatal("expected an error when no candidate image tag is supplied")
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_MissingDigestLineFailsClosed(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{
		inspectDigest: fakeDigestA + "\n",
		createOut:     "created pix-uatenv-fixture-image (positively identified)\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "docker.io/mcavage/pix:uat-test")
	if err == nil {
		t.Fatal("expected an error when the create log never reports the used image digest")
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
