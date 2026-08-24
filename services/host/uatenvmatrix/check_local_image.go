package uatenvmatrix

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// candidateImageFixtureNamePrefix is the fixed portion of every fixture name
// candidateImageFixtureName derives. It still starts with uatenvFixturePrefix
// (cleanup.go's own namespace guard), plus this check's own "fixture-image-"
// segment so a bounded artifact or a stray `sbx ls` line is still readable
// as "the local-candidate-image check's fixture".
const candidateImageFixtureNamePrefix = "pix-uatenv-fixture-image-"

// candidateImageFixtureNameMaxLen mirrors sandbox/name.go's own MaxNameLen
// (63, the strictest common DNS-label-style bound in play) — this package
// deliberately does not import the sandbox package (an L1 sibling; matrix.go's
// package doc forbids sideways L1 imports), so the bound is duplicated here
// as its own small, independently-tested constant.
const candidateImageFixtureNameMaxLen = 63

// sanitizeImageTagSegment keeps a run-unique image tag segment safe as a
// sandbox-name segment: only lowercase [a-z0-9-] survive (uppercase folds to
// lowercase rather than becoming '-', since a real UAT tag's timestamp/hash
// segments are already lowercase hex and digits), anything else becomes
// '-', and leading/trailing '-' are trimmed so a name never starts or ends
// on the separator this function introduces.
func sanitizeImageTagSegment(segment string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(segment) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// candidateImageFixtureName derives the deterministic, run-unique `pix-*`
// sandbox name this check's environment creates as, from the candidate
// image tag itself (never a fixed literal): a prior UAT run's own fixture
// name never leaked ("run-unique" and never published, per E0.7) can
// therefore never collide with — or block — a later run's own attempt at
// the SAME check, and two concurrent/retried runs against two DIFFERENT
// candidate tags can never collide with each other either. Only the portion
// of imageTag after the last ':' (the tag itself, e.g.
// "uat-run-20260824-092338-d4c384f5") is used — the repository segment
// ("docker.io/mcavage/pix") is identical across every run and would add
// nothing but wasted budget toward candidateImageFixtureNameMaxLen.
//
// Calling this twice for the SAME imageTag yields the SAME name
// (deterministic); this is required so cleanupCreatedFixture's own fresh
// `sbx ls --json` probe (issued after create, from this same derived name)
// still finds the exact instance this check just created.
func candidateImageFixtureName(imageTag string) string {
	tag := imageTag
	if i := strings.LastIndex(imageTag, ":"); i >= 0 {
		tag = imageTag[i+1:]
	}
	sanitized := sanitizeImageTagSegment(tag)
	budget := candidateImageFixtureNameMaxLen - len(candidateImageFixtureNamePrefix)
	if budget < 1 {
		budget = 1
	}
	if sanitized == "" {
		sanitized = "untagged"
	}
	if len(sanitized) > budget {
		// Keep the trailing budget characters, not the leading ones: a real
		// UAT tag's run-unique entropy (the trailing short hash) lives at
		// the END of the tag, after the human-legible "uat-run-<timestamp>-"
		// prefix every run shares, so truncating from the front preserves
		// the part two DIFFERENT runs are least likely to share.
		sanitized = sanitized[len(sanitized)-budget:]
		sanitized = strings.TrimLeft(sanitized, "-")
		if sanitized == "" {
			sanitized = "x"
		}
	}
	return candidateImageFixtureNamePrefix + sanitized
}

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
func candidateImageFixtureYAML(name, imageTag string) []byte {
	return []byte(fmt.Sprintf(`schemaVersion: "1"
agent: pix
name: %s

kits:
  - ./kit

sandboxOptions:
  template: %s
  pullPolicy: missing
`, name, imageTag))
}

// candidateImageFixture is the one fixture
// checkEnvironmentUsesLocalCandidateImage exercises, materialized through
// writeAuthoredFixture exactly like every other fixture in this package
// that declares `agent: pix`: RelativeKits ensures the referenced kit is
// materialized with kit-spec name "pix", so sbx's own agent/kit identity
// check passes and sandboxOptions.template + pullPolicy: missing still
// selects the UAT candidate image. Its Name is derived (never fixed) by
// candidateImageFixtureName, so a leaked prior-run fixture can never block a
// later run's own attempt at this check.
func candidateImageFixture(imageTag string) EnvironmentFixture {
	name := candidateImageFixtureName(imageTag)
	return EnvironmentFixture{
		Name:         name,
		YAML:         candidateImageFixtureYAML(name, imageTag),
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

// splitImageRef splits a fully qualified "repo:tag" candidate image
// reference into its repository and tag portions on the LAST colon, so a
// registry host:port prefix in repo (not used by this package's own
// candidate tags today, but a generally valid image-ref shape) is never
// mistaken for the tag separator.
func splitImageRef(ref string) (repo, tag string, ok bool) {
	i := strings.LastIndex(ref, ":")
	if i < 0 || i == len(ref)-1 || i == 0 {
		return "", "", false
	}
	return ref[:i], ref[i+1:], true
}

// templateListRow is one parsed row of `sbx template ls`'s tabular
// REPOSITORY/TAG output.
type templateListRow struct {
	Repository string
	Tag        string
}

// parseTemplateListRepoTag parses `sbx template ls`'s header-driven table
// into typed rows, locating the REPOSITORY and TAG columns by their header
// text (case-insensitive) rather than assuming a fixed column index or
// scanning for a joined "repo:tag" substring anywhere in the output — the
// read-only investigation's own finding this check corrects for: a joined
// substring match could be fooled by one row's repository ending in the
// exact characters another row's tag begins with, or by the same text
// appearing incidentally elsewhere in the table. A row that does not have
// enough fields to fill both located columns is skipped rather than
// guessed at.
func parseTemplateListRepoTag(out string) ([]templateListRow, error) {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("sbx template ls: empty output, no header row")
	}
	header := strings.Fields(lines[0])
	repoIdx, tagIdx := -1, -1
	for i, f := range header {
		switch strings.ToUpper(f) {
		case "REPOSITORY":
			repoIdx = i
		case "TAG":
			tagIdx = i
		}
	}
	if repoIdx < 0 || tagIdx < 0 {
		return nil, fmt.Errorf("sbx template ls: header %q is missing a REPOSITORY and/or TAG column", lines[0])
	}
	var rows []templateListRow
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) <= repoIdx || len(fields) <= tagIdx {
			continue
		}
		rows = append(rows, templateListRow{Repository: fields[repoIdx], Tag: fields[tagIdx]})
	}
	return rows, nil
}

// templateListHasExactRepoTag reports whether out's parsed rows contain a
// row whose Repository and Tag both match exactly — never a substring or
// prefix match on either column.
func templateListHasExactRepoTag(out, repo, tag string) (bool, error) {
	rows, err := parseTemplateListRepoTag(out)
	if err != nil {
		return false, err
	}
	for _, row := range rows {
		if row.Repository == repo && row.Tag == tag {
			return true, nil
		}
	}
	return false, nil
}

// prepareImageSectionHeaderPrefix is the literal box-drawing marker a real
// `sbx env create` receipt uses to open a named section. Fresh UAT run
// run-20260824-092338-d4c384f5's own verbatim receipt is TWO separate
// lines, never one joined line:
//
//	── PREPARE IMAGE
//	   → check <exact tag>
//	   ✓ image ready
//
// A prior version of this parser invented a single joined
// "PREPARE IMAGE \u2192 check <ref>" line that this real shape can never
// match.
const prepareImageSectionHeaderPrefix = "\u2500\u2500 "

// prepareImageSectionTitle is the exact section title following
// prepareImageSectionHeaderPrefix for the image-preparation section.
const prepareImageSectionTitle = "PREPARE IMAGE"

// prepareImageCheckLinePrefix is the literal marker a real receipt's own
// indented body line uses, inside the PREPARE IMAGE section, to name the
// exact image reference being checked.
const prepareImageCheckLinePrefix = "\u2192 check "

// extractPrepareImageCheckRef returns the exact image reference named on
// createOut's own PREPARE IMAGE section body line ("\u2192 check <ref>"),
// or "", false if that section — or a check line within it — is never
// present. It is deliberately section-aware: a line reading "\u2192 check
// <ref>" that appears OUTSIDE the PREPARE IMAGE section (under some other
// section header, or before any header is ever opened) is never matched —
// only an indented body line belonging to that exact section counts. This
// mirrors the real receipt's own two-line shape (a section header line,
// then its own indented body lines below it) rather than scanning for the
// check marker as a bare substring anywhere in the output.
func extractPrepareImageCheckRef(createOut string) (string, bool) {
	inSection := false
	for _, line := range strings.Split(createOut, "\n") {
		trimmed := strings.TrimSpace(line)
		if title, ok := strings.CutPrefix(trimmed, prepareImageSectionHeaderPrefix); ok {
			inSection = strings.TrimSpace(title) == prepareImageSectionTitle
			continue
		}
		if !inSection {
			continue
		}
		if line != "" && !isIndentedReceiptLine(line) {
			// A non-indented, non-header line ends this section's body.
			inSection = false
			continue
		}
		if rest, ok := strings.CutPrefix(trimmed, prepareImageCheckLinePrefix); ok {
			ref := strings.TrimSpace(rest)
			if ref != "" {
				return ref, true
			}
		}
	}
	return "", false
}

// isIndentedReceiptLine reports whether line (before trimming) starts with
// leading whitespace — the real receipt's own convention for a section's
// body lines, as opposed to a section header or an unrelated top-level
// line.
func isIndentedReceiptLine(line string) bool {
	return len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
}

// candidateRepoImageRefPattern builds the regexp matching every
// "<repo>:<tag-ish>" occurrence for one specific repository, using a
// plausible tag character set ([A-Za-z0-9._-]+, the set a real Docker tag
// is restricted to).
func candidateRepoImageRefPattern(repo string) *regexp.Regexp {
	return regexp.MustCompile(regexp.QuoteMeta(repo) + `:[A-Za-z0-9._-]+`)
}

// validateCreateReceiptResolvedImage is the pure, independently-testable
// core of AC-2's create-receipt evidence (E0.7's acceptance-evidence
// correction): createOut's own "PREPARE IMAGE \u2192 check <ref>" line must
// name EXACTLY imageTag — never missing outright, and never a substituted
// tag for the same repository — and every OTHER place createOut mentions
// the candidate repository must use that identical tag too. A mixed
// reference (the candidate repository appearing anywhere in the receipt
// with any different tag) is exactly the ambiguity this check refuses to
// treat as proof that the environment used the pinned local candidate.
func validateCreateReceiptResolvedImage(createOut, imageTag string) error {
	repo, _, ok := splitImageRef(imageTag)
	if !ok {
		return fmt.Errorf("candidate image tag %q is not a fully qualified repo:tag reference", imageTag)
	}
	ref, found := extractPrepareImageCheckRef(createOut)
	if !found {
		return fmt.Errorf("sbx env create receipt never printed a %q section with a %q line naming the resolved image", prepareImageSectionTitle, strings.TrimSpace(prepareImageCheckLinePrefix))
	}
	if ref != imageTag {
		return fmt.Errorf("sbx env create receipt resolved image %q, want the exact candidate tag %q", ref, imageTag)
	}
	for _, r := range candidateRepoImageRefPattern(repo).FindAllString(createOut, -1) {
		if r != imageTag {
			return fmt.Errorf("sbx env create receipt references %s with a different tag (%q) than the exact candidate tag %q; every candidate-repo image reference in the receipt must agree", repo, r, imageTag)
		}
	}
	return nil
}

// checkEnvironmentUsesLocalCandidateImage is Story 0's second named check
// (AC-2, docs/design/environments.md section 11): prove that a native
// environment pinned to the just-built, just-loaded candidate image starts
// from that exact local image — never a registry pull.
//
// A prior version of this check compared the local candidate image's ID
// against `docker inspect --format {{.Image}} <sandbox name>` — an invented
// observable. A read-only deep investigation (fresh UAT run
// run-20260824-092338-d4c384f5) proved an sbx sandbox is not a host-Docker
// container addressable by its sandbox name (`docker inspect
// pix-uatenv-fixture-image` => "no such object"), and that a correctness bug
// then leaked the fixture this invented probe created: the check reused
// `err` for that post-create Docker inspect call, so the deferred cleanup
// closure — which captures its createErr argument by reference — observed
// the LATER (post-create) call's error instead of the create's own, and
// misclassified a successful create as receiptless whenever that later call
// itself failed. This is the exact bug class check_create_exec.go already
// fixed by naming its create error result its own never-reassigned
// `createErr` identifier; this check now does the same (see createErr
// below), and immutable_create_err_test.go statically guards it.
//
// The read-only investigation also found no supported created-sandbox
// digest field: real `sbx ls --json` rows carry name/id/agent/status/
// workspaces only, never an image digest. The strongest observable proof
// sbx 0.39 exposes for a run-unique, never-published candidate tag is
// therefore reconstructed from what IS observable, in order:
//  1. record the local candidate digest (evidence only, via `docker image
//     inspect`; there is no created-sandbox digest to compare it against);
//  2. `sbx template ls` lists the run-unique candidate repo+tag BEFORE
//     create is ever attempted (parsed by REPOSITORY/TAG column, never a
//     joined substring match);
//  3. the fixture pins `sandboxOptions.template: <exact tag>` with
//     `pullPolicy: missing`;
//  4. the create receipt's own resolved-image line names that exact tag,
//     and every candidate-repository reference anywhere in the receipt
//     agrees (validateCreateReceiptResolvedImage);
//  5. no registry pull/download marker anywhere in stdout or stderr; and
//  6. a fresh `sbx ls --json` poll (pollForRunningInstance, the same
//     package-local bounded poll checkEnvironmentCreateThenExecInvocation
//     uses) observes the exact created instance running.
//
// docs/upstream/sbx-0.39-environments.md and Story 1's E0.7 will record
// that sandbox-digest equality is unobservable with sbx 0.39's `sbx ls
// --json` schema — an acceptance-evidence correction, not a design stop.
//
// Every host command goes through the injected Executor, exactly like
// checkEnvironmentCreateThenExecInvocation: no real `docker` or `sbx` binary
// is ever required under `go test`.
func checkEnvironmentUsesLocalCandidateImage(ctx context.Context, lw io.Writer, executor Executor, phaseDir string, imageTag string) (retErr error) {
	if imageTag == "" {
		return fmt.Errorf("environment_uses_local_candidate_image: no candidate image tag supplied (caller bug: Inputs.ImageTag must always be set)")
	}
	repo, tag, ok := splitImageRef(imageTag)
	if !ok {
		return fmt.Errorf("environment_uses_local_candidate_image: candidate image tag %q is not a fully qualified repo:tag reference", imageTag)
	}

	env := hostToolExecEnv()

	inspectArgs := []string{"image", "inspect", "--format", "{{.Id}}", imageTag}
	fmt.Fprintf(lw, "$ docker %s\n", strings.Join(inspectArgs, " "))
	localImageOut, localImageErrOut, localImageErr := executor.Run(ctx, "docker", inspectArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", localImageOut, localImageErrOut, localImageErr)
	if localImageErr != nil {
		return fmt.Errorf("docker image inspect %s: %w", imageTag, localImageErr)
	}
	localImageID := strings.TrimSpace(localImageOut)
	if localImageID == "" {
		return fmt.Errorf("docker image inspect %s returned no image ID", imageTag)
	}
	fmt.Fprintf(lw, "local candidate image ID: %s (evidence only; sbx 0.39 exposes no created-sandbox digest field to compare it against)\n", localImageID)

	templateListArgs := []string{"template", "ls"}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(templateListArgs, " "))
	templateListOut, templateListErrOut, templateListErr := executor.Run(ctx, "sbx", templateListArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", templateListOut, templateListErrOut, templateListErr)
	if templateListErr != nil {
		return fmt.Errorf("sbx template ls: %w", templateListErr)
	}
	hasTemplate, parseErr := templateListHasExactRepoTag(templateListOut, repo, tag)
	if parseErr != nil {
		return fmt.Errorf("sbx template ls: %w", parseErr)
	}
	if !hasTemplate {
		return fmt.Errorf("sbx template ls did not list the run-unique candidate template %s:%s before create was attempted (stdout=%q)", repo, tag, templateListOut)
	}
	fmt.Fprintf(lw, "sbx template ls confirmed %s:%s is registered before create\n", repo, tag)

	fixture := candidateImageFixture(imageTag)
	fixturePath, writeErr := writeAuthoredFixture(phaseDir, "candidate-image.sbxenv.yaml", fixture)
	if writeErr != nil {
		return writeErr
	}
	fmt.Fprintf(lw, "authored fixture written to %s\n", fixturePath)

	createArgs := []string{"env", "create", fixturePath}
	fmt.Fprintf(lw, "$ sbx %s\n", strings.Join(createArgs, " "))
	// createErr, like check_create_exec.go's own createErr, is deliberately
	// its own named variable, NEVER reused for a later call in this
	// function: cleanupCreatedFixture is gated on this create's own
	// receipt, and the deferred closure below captures createOut/createErr
	// by reference, so reassigning createErr for a later call (as this
	// check once did, reusing a bare `err` for its now-removed post-create
	// `docker inspect` call) would let that later call's own outcome
	// silently overwrite the create's by the time the deferred cleanup
	// actually runs — the exact leak fresh UAT run
	// run-20260824-092338-d4c384f5 found. immutable_create_err_test.go
	// statically guards that this identifier is assigned exactly once.
	createOut, createErrOut, createErr := executor.Run(ctx, "sbx", createArgs, env, phaseDir)
	fmt.Fprintf(lw, "stdout: %s\nstderr: %s\nerr: %v\n", createOut, createErrOut, createErr)
	defer func() {
		if cleanupErr := cleanupCreatedFixture(ctx, lw, executor, env, phaseDir, fixturePath, fixture.Name, createOut, createErr); cleanupErr != nil && retErr == nil {
			retErr = cleanupErr
		}
	}()
	if createErr != nil {
		return fmt.Errorf("sbx env create: %w", createErr)
	}
	if !strings.Contains(createOut, fixture.Name) {
		return fmt.Errorf("sbx env create did not report the expected positively-identified instance name %q (stdout=%q)", fixture.Name, createOut)
	}

	if err := validateCreateReceiptResolvedImage(createOut, imageTag); err != nil {
		return fmt.Errorf("create receipt image identity: %w", err)
	}
	fmt.Fprintf(lw, "create receipt resolved image %s, matching the exact candidate tag with no mixed reference\n", imageTag)

	combinedLog := createOut + "\n" + createErrOut
	for _, marker := range registryPullMarkers {
		if strings.Contains(combinedLog, marker) {
			return fmt.Errorf("sbx env create log contains a registry pull marker %q; the environment must start from the already-loaded local candidate image, never a registry", marker)
		}
	}
	fmt.Fprintf(lw, "no registry pull marker observed in stdout or stderr\n")

	if err := pollForRunningInstance(ctx, lw, executor, env, phaseDir, fixture.Name, runningRowPollConfig); err != nil {
		return err
	}

	fmt.Fprintf(lw, "environment_uses_local_candidate_image: %s registered as a template, resolved exactly in the create receipt, no registry pull observed, and confirmed running\n", imageTag)
	return nil
}
