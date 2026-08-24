// check_local_image_test.go proves environment_uses_local_candidate_image
// (AC-2, docs/design/environments.md section 11) in isolation, against an
// injected fake Executor and this check's own function — the same posture
// isolation_test.go uses for buildExecArgv, so this unit's tests never need
// to know about, or update, the first named check's Run()-level tests.
//
// Fresh UAT run run-20260824-092338-d4c384f5 found the PRIOR version of this
// check invalid on three counts: it compared the local candidate image's ID
// against `docker inspect --format {{.Image}} <sandbox name>`, but a real
// sbx sandbox is not a host-Docker container addressable by its sandbox name
// (`docker inspect pix-uatenv-fixture-image` => "no such object" against a
// real sbx-created instance); a correctness bug reused a bare `err` for that
// invented post-create Docker inspect call, so the deferred cleanup closure
// (which captures its createErr argument by reference) observed the LATER
// call's error instead of the create's own and misclassified a successful
// create as receiptless whenever that later call itself failed — leaking the
// fixture; and its fixed literal fixture name meant a leaked prior run's
// fixture could block a later run's own attempt at this same check. This
// file's tests are rewritten against the corrected contract: `sbx template
// ls` (parsed by REPOSITORY/TAG column) replaces the invented Docker
// container probe, the fixture name is derived deterministically from the
// candidate image tag (candidateImageFixtureName), and createErr is never
// reassigned after the create call (immutable_create_err_test.go statically
// guards this).
package uatenvmatrix

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

var (
	errTestLocalImageInspect = errors.New("fixture: docker image inspect failed")
	errTestTemplateList      = errors.New("fixture: sbx template ls failed")
	errTestCreate            = errors.New("fixture: sbx env create failed")
)

const fakeCandidateTag = "docker.io/mcavage/pix:uat-test"
const fakeCandidateRepo = "docker.io/mcavage/pix"
const fakeCandidateTagOnly = "uat-test"

// fakeTemplateListOut renders a realistic `sbx template ls` table
// (REPOSITORY/TAG columns, matching `docker images`'s own convention) that
// lists exactly the candidate repo:tag this file's tests exercise, plus one
// UNRELATED row whose repository/tag values are individually substrings of
// the candidate's own repo/tag — proving templateListHasExactRepoTag matches
// on the parsed columns, never a joined substring scan.
const fakeTemplateListOut = `REPOSITORY               TAG        IMAGE ID       CREATED         SIZE
docker.io/mcavage/pix    uat-test   sha256:aaaa    2 minutes ago   1.2GB
docker.io/mcavage/pi     x          sha256:eeee    3 days ago      1.1GB
`

// fakeTemplateListMissingCandidateOut lists templates, but never the
// candidate repo:tag pair this check requires be present before create.
const fakeTemplateListMissingCandidateOut = `REPOSITORY               TAG        IMAGE ID       CREATED         SIZE
docker.io/mcavage/pix    latest     sha256:bbbb    2 minutes ago   1.2GB
`

// fakeCreateOutSuccess is a realistic `sbx env create` receipt shape, per
// run-20260824-092338-d4c384f5's own observation: a "PREPARE IMAGE" line
// naming the exact resolved image, a neutral "image ready" confirmation
// once the already-loaded local image satisfies it, and a positively
// identified instance line. It intentionally never fabricates an "image
// digest: sha256:..." line — the prior version's own invalid assumption.
func fakeCreateOutSuccess(name string) string {
	return "PREPARE IMAGE \u2192 check " + fakeCandidateTag + "\n" +
		"\u2713 image ready\n" +
		"created " + name + " (positively identified)\n"
}

// localImageFakeExecutor answers `docker image inspect` (the local
// candidate image lookup), `sbx template ls`, `sbx env create`, the
// post-create `sbx ls --json` poll/cleanup probe, and `sbx env rm -f`
// deterministically — exactly the seam AC-2 requires: no real sbx or docker
// binary is ever invoked under go test.
type localImageFakeExecutor struct {
	localImageID  string
	localImageErr error
	templateList  string
	templateErr   error
	createOut     string
	createErrOut  string
	createErr     error
	lsOut         string
	rmErr         error
}

func (f localImageFakeExecutor) Run(ctx context.Context, name string, args, env []string, dir string) (string, string, error) {
	if name == "docker" {
		return f.localImageID, "", f.localImageErr
	}
	if len(args) > 1 && args[0] == "template" && args[1] == "ls" {
		return f.templateList, "", f.templateErr
	}
	if len(args) > 0 && args[0] == "ls" {
		out := f.lsOut
		if out == "" {
			out = f.createOut
		}
		return out, "", nil
	}
	if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
		return "removed\n", "", f.rmErr
	}
	return f.createOut, f.createErrOut, f.createErr
}

func defaultLocalImageFakeExecutor(fixtureName string) localImageFakeExecutor {
	return localImageFakeExecutor{
		localImageID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		templateList: fakeTemplateListOut,
		createOut:    fakeCreateOutSuccess(fixtureName),
		lsOut:        `[{"name":"` + fixtureName + `","status":"running"}]` + "\n",
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_Success(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := localImageFakeExecutor{
		localImageID: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		templateList: fakeTemplateListOut,
		createOut:    fakeCreateOutSuccess(name),
		lsOut:        `[{"name":"` + name + `","status":"running"}]` + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !strings.Contains(lw.String(), fakeCandidateTag) {
		t.Errorf("log does not record the candidate tag: %s", lw.String())
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_NoImageTagFailsClosed(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{localImageID: "sha256:aaaa\n"}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "")
	if err == nil {
		t.Fatal("expected an error when no candidate image tag is supplied")
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_MalformedImageTagFailsClosed(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := localImageFakeExecutor{localImageID: "sha256:aaaa\n"}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, "no-colon-here")
	if err == nil {
		t.Fatal("expected an error for a candidate image tag with no repo:tag separator")
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_LocalImageInspectFailureFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.localImageErr = errTestLocalImageInspect
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when docker image inspect fails")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_MissingTemplateFails proves
// the check fails closed BEFORE ever attempting create when `sbx template
// ls` does not list the run-unique candidate repo:tag.
func TestCheckEnvironmentUsesLocalCandidateImage_MissingTemplateFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.templateList = fakeTemplateListMissingCandidateOut
	var createCalled bool
	wrapped := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 1 && args[0] == "env" && args[1] == "create" {
			createCalled = true
		}
		out, errOut, err := executor.Run(context.Background(), "sbx", args, nil, "")
		return out, errOut, err
	}}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, wrapped, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when sbx template ls does not list the candidate repo:tag")
	}
	if !strings.Contains(err.Error(), "template ls") {
		t.Errorf("error does not name the template ls check: %v", err)
	}
	if createCalled {
		t.Error("sbx env create must never be attempted when the candidate template is not listed")
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_TemplateListErrorFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.templateErr = errTestTemplateList
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when sbx template ls itself fails")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_UnparsableTemplateListFails
// proves a header missing REPOSITORY/TAG columns fails closed rather than
// being silently treated as "candidate not found, but also not an error".
func TestCheckEnvironmentUsesLocalCandidateImage_UnparsableTemplateListFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.templateList = "NAME  ID\nfoo   bar\n"
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when sbx template ls output cannot be parsed for REPOSITORY/TAG columns")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_RunningRowNeverObservedFails
// proves the check fails closed when the post-create poll for a positively
// identified running row times out (the package-local bounded poll helper,
// pollForRunningInstance, injected with a fast bound via
// runningRowPollConfig for this test).
func TestCheckEnvironmentUsesLocalCandidateImage_RunningRowNeverObservedFails(t *testing.T) {
	origCfg := runningRowPollConfig
	runningRowPollConfig = pollConfig{Interval: time.Millisecond, Timeout: 20 * time.Millisecond}
	defer func() { runningRowPollConfig = origCfg }()

	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := localImageFakeExecutor{
		localImageID: "sha256:aaaa\n",
		templateList: fakeTemplateListOut,
		createOut:    fakeCreateOutSuccess(name),
		lsOut:        `[{"name":"` + name + `","status":"stopped"}]` + "\n",
	}
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when the created instance is never observed running")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_RegistryPullInStdoutFails and
// its stderr twin prove the negative pull-marker scan covers BOTH streams
// independently, never only the one the prior version of this check
// happened to concatenate first.
func TestCheckEnvironmentUsesLocalCandidateImage_RegistryPullInStdoutFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.createOut = "Pulling from docker.io/mcavage/pix\n" + fakeCreateOutSuccess(name)
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected a registry-pull error from stdout, got nil")
	}
	if !strings.Contains(err.Error(), "registry pull") {
		t.Fatalf("error does not name the registry pull: %v", err)
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_RegistryPullInStderrFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.createErrOut = "Download complete\n"
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected a registry-pull error from stderr, got nil")
	}
	if !strings.Contains(err.Error(), "registry pull") {
		t.Fatalf("error does not name the registry pull: %v", err)
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_NeutralImageTextNotClassifiedAsPull
// proves the negative scan does not misclassify the REAL neutral create-log
// text run-20260824-092338-d4c384f5 observed around an already-present
// local image ("PREPARE IMAGE", "image ready") as a registry pull.
func TestCheckEnvironmentUsesLocalCandidateImage_NeutralImageTextNotClassifiedAsPull(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err != nil {
		t.Fatalf("neutral create-log text must not be classified as a registry pull, got: %v", err)
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_MissingResolvedImageLineFails
// proves a create receipt lacking the "PREPARE IMAGE" line at all is
// rejected, never silently accepted merely because the instance name was
// positively identified.
func TestCheckEnvironmentUsesLocalCandidateImage_MissingResolvedImageLineFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.createOut = "created " + name + " (positively identified)\n"
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when the receipt never names a resolved image")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_SubstitutedTagFails proves a
// receipt that resolves a DIFFERENT tag for the same candidate repository
// (e.g. "latest" instead of the exact pinned tag) is rejected.
func TestCheckEnvironmentUsesLocalCandidateImage_SubstitutedTagFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.createOut = "PREPARE IMAGE \u2192 check " + fakeCandidateRepo + ":latest\n" +
		"\u2713 image ready\n" +
		"created " + name + " (positively identified)\n"
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when the receipt resolves a substituted tag")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_MixedRepoReferenceFails proves
// a receipt whose resolved-image line correctly names the exact candidate
// tag, but that ALSO mentions the same repository with a different tag
// elsewhere, is rejected — every candidate-repo reference in the receipt
// must agree.
func TestCheckEnvironmentUsesLocalCandidateImage_MixedRepoReferenceFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.createOut = fakeCreateOutSuccess(name) + "cached layer for " + fakeCandidateRepo + ":latest\n"
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when the receipt mixes a different tag for the same candidate repository")
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_CreateFailureFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := defaultLocalImageFakeExecutor(name)
	executor.createErr = errTestCreate
	executor.createOut = ""
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when sbx env create fails")
	}
}

func TestCheckEnvironmentUsesLocalCandidateImage_UnidentifiedInstanceFails(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	executor := defaultLocalImageFakeExecutor(candidateImageFixtureName(fakeCandidateTag))
	executor.createOut = "accepted\n"
	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected an error when create never positively identifies the fixture instance")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_TemplateListArgvShape pins the
// exact argv this check issues to prove the candidate template is
// registered: `sbx template ls`, issued BEFORE `sbx env create`.
func TestCheckEnvironmentUsesLocalCandidateImage_TemplateListArgvShape(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	var calls [][]string
	name := candidateImageFixtureName(fakeCandidateTag)
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		calls = append(calls, append([]string(nil), args...))
		if len(args) > 0 && args[0] == "image" {
			return "sha256:aaaa\n", "", nil
		}
		if len(args) > 1 && args[0] == "template" && args[1] == "ls" {
			return fakeTemplateListOut, "", nil
		}
		if len(args) > 0 && args[0] == "ls" {
			return `[{"name":"` + name + `","status":"running"}]` + "\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			return "removed\n", "", nil
		}
		return fakeCreateOutSuccess(name), "", nil
	}}
	if err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	var templateListIdx, createIdx = -1, -1
	for i, c := range calls {
		if len(c) > 1 && c[0] == "template" && c[1] == "ls" {
			templateListIdx = i
		}
		if len(c) > 1 && c[0] == "env" && c[1] == "create" {
			createIdx = i
		}
	}
	if templateListIdx < 0 {
		t.Fatal("expected an `sbx template ls` call")
	}
	if createIdx < 0 {
		t.Fatal("expected an `sbx env create` call")
	}
	if templateListIdx > createIdx {
		t.Errorf("sbx template ls must be issued BEFORE sbx env create: template ls at %d, create at %d", templateListIdx, createIdx)
	}
	if len(calls[templateListIdx]) != 2 {
		t.Errorf("sbx template ls call = %#v, want exactly [template ls]", calls[templateListIdx])
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_NeverInvokesDockerInspectByName
// is the direct regression test for the invented observable this check must
// never reintroduce: no `docker inspect <sandbox name>` call (an sbx sandbox
// is not a host-Docker container addressable by its sandbox name).
func TestCheckEnvironmentUsesLocalCandidateImage_NeverInvokesDockerInspectByName(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	var dockerCalls [][]string
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "image" {
			dockerCalls = append(dockerCalls, append([]string(nil), args...))
			return "sha256:aaaa\n", "", nil
		}
		if len(args) > 0 && args[0] == "inspect" {
			dockerCalls = append(dockerCalls, append([]string(nil), args...))
			return "sha256:aaaa\n", "", nil
		}
		if len(args) > 1 && args[0] == "template" && args[1] == "ls" {
			return fakeTemplateListOut, "", nil
		}
		if len(args) > 0 && args[0] == "ls" {
			return `[{"name":"` + name + `","status":"running"}]` + "\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			return "removed\n", "", nil
		}
		return fakeCreateOutSuccess(name), "", nil
	}}
	if err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	for _, c := range dockerCalls {
		if len(c) > 0 && c[0] == "inspect" {
			t.Fatalf("unexpected `docker inspect <name>` call (the invented observable this check must never reintroduce): %#v", c)
		}
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_CleanupRunsAndPrimaryErrorWins
// proves receipt-gated cleanup still runs (a fresh `sbx ls --json` probe,
// then `sbx env rm -f`) even when the check itself fails on a
// post-create facet (here: a registry pull marker), and that the primary
// error — not cleanup's own nil result — is what propagates.
func TestCheckEnvironmentUsesLocalCandidateImage_CleanupRunsAndPrimaryErrorWins(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	var sawLsProbe, sawRemove bool
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "image" {
			return "sha256:aaaa\n", "", nil
		}
		if len(args) > 1 && args[0] == "template" && args[1] == "ls" {
			return fakeTemplateListOut, "", nil
		}
		if len(args) > 0 && args[0] == "ls" {
			sawLsProbe = true
			return `[{"name":"` + name + `","status":"running"}]` + "\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			sawRemove = true
			return "removed\n", "", nil
		}
		return "Pulling from docker.io/mcavage/pix\n" + fakeCreateOutSuccess(name), "", nil
	}}

	err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag)
	if err == nil {
		t.Fatal("expected the registry-pull error to propagate")
	}
	if !strings.Contains(err.Error(), "registry pull") {
		t.Fatalf("Run's reported error must be the primary registry-pull error, got: %v", err)
	}
	if !sawLsProbe {
		t.Error("expected cleanup's fresh `sbx ls --json` probe to run despite the check's own failure")
	}
	if !sawRemove {
		t.Error("expected cleanup's `sbx env rm -f` to run despite the check's own failure")
	}
}

// TestCheckEnvironmentUsesLocalCandidateImage_FullSuccessCleansUp proves the
// full happy path also runs receipt-gated cleanup to completion (fresh
// probe + removal), never leaving a positively-identified instance behind
// merely because the check itself passed.
func TestCheckEnvironmentUsesLocalCandidateImage_FullSuccessCleansUp(t *testing.T) {
	phaseDir := t.TempDir()
	var lw strings.Builder
	name := candidateImageFixtureName(fakeCandidateTag)
	var sawLsProbe, sawRemove bool
	executor := recordingExecutor{fn: func(args []string) (string, string, error) {
		if len(args) > 0 && args[0] == "image" {
			return "sha256:aaaa\n", "", nil
		}
		if len(args) > 1 && args[0] == "template" && args[1] == "ls" {
			return fakeTemplateListOut, "", nil
		}
		if len(args) > 0 && args[0] == "ls" {
			sawLsProbe = true
			return `[{"name":"` + name + `","status":"running"}]` + "\n", "", nil
		}
		if len(args) > 1 && args[0] == "env" && args[1] == "rm" {
			sawRemove = true
			return "removed\n", "", nil
		}
		return fakeCreateOutSuccess(name), "", nil
	}}
	if err := checkEnvironmentUsesLocalCandidateImage(context.Background(), &lw, executor, phaseDir, fakeCandidateTag); err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !sawLsProbe {
		t.Error("expected cleanup's fresh `sbx ls --json` probe to run on the success path")
	}
	if !sawRemove {
		t.Error("expected cleanup's `sbx env rm -f` to run on the success path")
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
