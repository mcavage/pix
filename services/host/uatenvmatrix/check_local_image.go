package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// candidateImageFixtureName is the literal pix-* sandbox name this check's
// environment creates as — Story 0 owns naming directly here, the same way
// checkEnvironmentCreateThenExecInvocation's fixture does (fixtures.go).
const candidateImageFixtureName = "pix-uatenv-fixture-image"

// candidateImageFixtureYAML renders the one authored declaration this check
// exercises: a native environment whose `agent: pix` selects the candidate
// custom Pix agent kit — declared via a relative `kits:` entry, exactly
// like every other `agent: pix` fixture in this package
// (customAgentFixture, ollamaCapabilityFixture) — pinned to the exact
// candidate image this UAT run just built and loaded locally (imageTag) via
// sandboxOptions.template with pullPolicy: missing so sbx must use the
// local image rather than reach a registry — the literal ownership
// boundary docs/design/environments.md section 6.2 documents ("pinned Pix
// template and pullPolicy: missing"). It is a package-owned literal, never
// derived from envinfo's renderer.
//
// Host UAT run run-20260824-091306-29559f3a hit `ERROR: "pix" is not a
// known agent` because this fixture declared `agent: pix` with no kit at
// all: a real `sbx env create` refuses an `agent: pix` declaration outright
// unless a referenced kit resolves to a materialized kit-spec whose own
// declared name is "pix" (the same fix run-20260824-082317-e58d0587 already
// established for customAgentFixture/ollamaCapabilityFixture). Declaring
// `kits: [./kit]` here, and routing this fixture through writeAuthoredFixture
// (see candidateImageFixture below), closes that gap the same way.
func candidateImageFixtureYAML(imageTag string) []byte {
	return []byte(fmt.Sprintf(`schemaVersion: "1"
agent: pix
name: %s

kits:
  - ./kit

sandboxOptions:
  template: %s
  pullPolicy: missing
`, candidateImageFixtureName, imageTag))
}

// candidateImageFixture is the one fixture
// checkEnvironmentUsesLocalCandidateImage exercises, materialized through
// writeAuthoredFixture exactly like every other fixture in this package
// that declares `agent: pix`: RelativeKits ensures the referenced kit is
// materialized with kit-spec name "pix", so sbx's own agent/kit identity
// check passes and sandboxOptions.template + pullPolicy: missing still
// selects the UAT candidate image.
func candidateImageFixture(imageTag string) EnvironmentFixture {
	return EnvironmentFixture{
		Name:         candidateImageFixtureName,
		YAML:         candidateImageFixtureYAML(imageTag),
		RelativeKits: []string{"./kit"},
	}
}

// registryPullMarkers are literal substrings a real `sbx env create` log
// would contain only if it reached a registry instead of the already-loaded
// local image store — the exact negative evidence AC-2 requires be absent
// from the observed create log.
var registryPullMarkers = []string{
	"Pulling from",
	"Pull complete",
	"Download complete",
	"Downloading",
	"pulling image",
}

// checkEnvironmentUsesLocalCandidateImage is Story 0's second named check
// (AC-2, docs/design/environments.md section 11): prove that a native
// environment pinned to the just-built, just-loaded candidate image starts
// from that exact local image — never a registry pull — by comparing the
// locally loaded candidate image's ID against the actually created
// sandbox's own image ID, and by scanning the create log for registry-pull
// evidence.
//
// The actual image identity comparison is a SECOND, independent Executor
// call issued after the create receipt (`docker inspect --format
// {{.Image}} <exact sandbox name>`) — never parsed out of the create log.
// Host UAT run run-20260824-091306-29559f3a's preceding check
// (environment_create_then_exec_invocation) proved a real `sbx env create`
// reports the selected image/tag and layer presence, never a fabricated
// `image digest: sha256:...` line, so asserting on one here would only ever
// pass against a scripted test fake. Addressing the container by the exact
// sandbox name is the narrowest, established observable this repo has for
// it: sandbox/list.go's own `container_name` key alias documents that a
// listed sandbox's underlying container is addressable by the sandbox's
// own name.
//
// Every host command goes through the injected Executor, exactly like
// checkEnvironmentCreateThenExecInvocation: no real `docker` or `sbx` binary
// is ever required under `go test`.
func checkEnvironmentUsesLocalCandidateImage(ctx context.Context, lw io.Writer, executor Executor, phaseDir string, imageTag string) (retErr error) {
	if imageTag == "" {
		return fmt.Errorf("environment_uses_local_candidate_image: no candidate image tag supplied (caller bug: Inputs.ImageTag must always be set)")
	}

	env := hostToolExecEnv()

	inspectArgs := []string{"image", "inspect", "--format", "{{.Id}}", imageTag}
	fmt.Fprintf(lw, "$ docker %s\n", strings.Join(inspectArgs, " "))
	inspectOut, inspectErrOut, err := executor.Run(ctx, "docker", inspectArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", inspectOut, inspectErrOut, err)
	if err != nil {
		return fmt.Errorf("docker image inspect %s: %w", imageTag, err)
	}
	localImageID := strings.TrimSpace(inspectOut)
	if localImageID == "" {
		return fmt.Errorf("docker image inspect %s returned no image ID", imageTag)
	}

	fixture := candidateImageFixture(imageTag)
	fixturePath, err := writeAuthoredFixture(phaseDir, "candidate-image.sbxenv.yaml", fixture)
	if err != nil {
		return err
	}
	fmt.Fprintf(lw, "authored fixture written to %s\n", fixturePath)

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	createOut, createErrOut, err := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, err)
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, fixture.Name, createOut, err); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()
	if err != nil {
		return fmt.Errorf("sbx env create: %w", err)
	}
	if !strings.Contains(createOut, fixture.Name) {
		return fmt.Errorf("sbx env create did not report the expected positively-identified instance name %q (stdout=%q)", fixture.Name, createOut)
	}

	combinedLog := createOut + "\n" + createErrOut
	for _, marker := range registryPullMarkers {
		if strings.Contains(combinedLog, marker) {
			return fmt.Errorf("sbx env create log contains a registry pull marker %q; the environment must start from the already-loaded local candidate image, never a registry", marker)
		}
	}

	actualInspectArgs := []string{"inspect", "--format", "{{.Image}}", fixture.Name}
	fmt.Fprintf(lw, "$ docker %s\n", strings.Join(actualInspectArgs, " "))
	actualOut, actualErrOut, err := executor.Run(ctx, "docker", actualInspectArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", actualOut, actualErrOut, err)
	if err != nil {
		return fmt.Errorf("docker inspect %s (actual created sandbox image identity): %w", fixture.Name, err)
	}
	actualImageID := strings.TrimSpace(actualOut)
	if actualImageID == "" {
		return fmt.Errorf("docker inspect %s returned no image ID for the actually created sandbox", fixture.Name)
	}
	if actualImageID != localImageID {
		return fmt.Errorf("created sandbox %s image ID = %q, want the locally loaded candidate image ID %q", fixture.Name, actualImageID, localImageID)
	}
	fmt.Fprintf(lw, "local candidate image ID %s matches the actually created sandbox %s; no registry pull observed\n", localImageID, fixture.Name)

	return nil
}
