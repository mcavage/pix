package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// candidateImageFixtureName is the literal pix-* sandbox name this check's
// environment creates as — Story 0 owns naming directly here, the same way
// checkEnvironmentCreateThenExecInvocation's fixture does (fixtures.go).
const candidateImageFixtureName = "pix-uatenv-fixture-image"

// candidateImageFixtureYAML renders the one authored declaration this check
// exercises: a native environment pinned to the exact candidate image this
// UAT run just built and loaded locally (imageTag), with pullPolicy: missing
// so sbx must use the local image rather than reach a registry — the literal
// ownership boundary docs/design/environments.md section 6.2 documents
// ("pinned Pix template and pullPolicy: missing"). It is a package-owned
// literal, like customAgentFixture, never derived from envinfo's renderer.
func candidateImageFixtureYAML(imageTag string) []byte {
	return []byte(fmt.Sprintf(`schemaVersion: "1"
agent: pix

sandboxOptions:
  template: %s
  pullPolicy: missing
`, imageTag))
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

// createdImageDigestPattern extracts the image digest a create log reports
// having actually used to start the sandbox. checkEnvironmentUsesLocalCandidateImage
// fails closed with a parse error when this line is absent, rather than
// silently skipping the digest comparison AC-2 requires.
var createdImageDigestPattern = regexp.MustCompile(`image[ _]?digest[:=]\s*(sha256:[0-9a-f]{64})`)

// checkEnvironmentUsesLocalCandidateImage is Story 0's second named check
// (AC-2, docs/design/environments.md section 11): prove that a native
// environment pinned to the just-built, just-loaded candidate image starts
// from that exact local image — never a registry pull — by comparing the
// locally loaded image's digest against the digest the observed `sbx env
// create` log reports having actually used, and by scanning that same log
// for registry-pull evidence.
//
// Every host command goes through the injected Executor, exactly like
// checkEnvironmentCreateThenExecInvocation: no real `docker` or `sbx` binary
// is ever required under `go test`.
func checkEnvironmentUsesLocalCandidateImage(ctx context.Context, lw io.Writer, executor Executor, phaseDir string, imageTag string) error {
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
	localDigest := strings.TrimSpace(inspectOut)
	if localDigest == "" {
		return fmt.Errorf("docker image inspect %s returned no digest", imageTag)
	}

	fixturePath := filepath.Join(phaseDir, "candidate-image.sbxenv.yaml")
	if err := os.WriteFile(fixturePath, candidateImageFixtureYAML(imageTag), 0600); err != nil {
		return fmt.Errorf("write candidate-image fixture: %w", err)
	}
	fmt.Fprintf(lw, "authored fixture written to %s\n", fixturePath)

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	createOut, createErrOut, err := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, err)
	if err != nil {
		return fmt.Errorf("sbx env create: %w", err)
	}

	combinedLog := createOut + "\n" + createErrOut
	for _, marker := range registryPullMarkers {
		if strings.Contains(combinedLog, marker) {
			return fmt.Errorf("sbx env create log contains a registry pull marker %q; the environment must start from the already-loaded local candidate image, never a registry", marker)
		}
	}

	match := createdImageDigestPattern.FindStringSubmatch(combinedLog)
	if match == nil {
		return fmt.Errorf("sbx env create log did not report the created sandbox's image digest (log=%q)", combinedLog)
	}
	createdDigest := match[1]
	if createdDigest != localDigest {
		return fmt.Errorf("created sandbox image digest = %q, want the locally loaded candidate digest %q", createdDigest, localDigest)
	}
	fmt.Fprintf(lw, "local candidate digest %s matches created sandbox digest; no registry pull observed\n", localDigest)

	return nil
}
