package uatenvmatrix

// fixtures.go owns every literal `.sbxenv.yaml` byte this matrix uses. These
// bytes are hand-authored against the upstream schema documented in
// docs/design/environments.md section 5.1 (schemaVersion "1", agent: pix,
// kits/sandboxOptions/env/secrets/bindings/mcp/ports) — they are NOT rendered
// by envinfo (the L1 capability Story 1 builds), which this package must not
// import (see matrix.go's package doc and arch_test.go). If a future
// envinfo-rendered effective file ever happens to agree with a fixture here,
// that agreement is evidence the renderer is faithful to the upstream
// contract; it would be a tautology if this package asked envinfo to build
// its own test fixture.
//
// EnvironmentFixture also carries the typed, Pix-owned facts a name-based
// `sbx exec` invocation must reproduce once a native environment is live:
// the generated kit, personal context, live skill trees, the exact model,
// and resume arguments (docs/design/environments.md section 11, item 2).
// checks build the exact expected exec argv from these fields with their own
// small, independent builder (buildExecArgv in check_create_exec.go) — never
// by importing workflow/launch's real argv builder or the sandbox package,
// for the same non-tautology reason.
type EnvironmentFixture struct {
	// RelativeKits is the exact set of relative paths this fixture's YAML
	// declares under its own top-level `kits:` list (fixtures.go's package
	// doc: these are literal, hand-authored bytes, so nothing parses YAML to
	// discover them). writeAuthoredFixture materializes each one as a real
	// directory next to the authored file, rooted the SAME way sbx itself
	// resolves a relative kit reference: relative to the `.sbxenv.yaml`
	// file's own directory, never the process's working directory. A
	// fixture that declares no `kits:` at all leaves this nil.
	RelativeKits []string
	// Name is the literal `pix-*` sandbox name this fixture's environment
	// creates as. Story 0 owns this name directly (a registered environment
	// normally omits `name`; Pix's own naming algorithm belongs to Story 1's
	// envinfo, not to this fixture) — which is exactly why every fixture's
	// YAML below declares an explicit top-level `name: <Name>` field: unlike
	// a Pix-composed effective file, nothing else tells a real `sbx env
	// create` which name to use, and every check in this package asserts
	// that the create receipt positively identifies this exact literal
	// value.
	Name string
	// YAML is the exact bytes written to disk as the authored native
	// declaration and passed to `sbx env create`.
	YAML []byte
	// Kit is the one candidate-generated kit path the custom `agent: pix`
	// exec invocation must carry.
	Kit string
	// LiveSkills is the ordered set of extra live skill trees (generated kit
	// skills plus personal context skills) the exec invocation must carry,
	// one `--skill DIR` per entry, in order.
	LiveSkills []string
	// Model is the exact session model the exec invocation must carry.
	Model string
	// Resume is the exact resume argument the exec invocation must carry.
	Resume string
}

// interpolationDefinedHostVar is the exact host environment variable name
// interpDefinedDefaultFixtureYAML references as a plain, defined
// interpolation (`${VAR}`) — AC-7's first required interpolation case. The
// interpolation observation phase (check_create_exec_interpolation.go)
// explicitly sets this key on the create call's own Executor env, to a
// known value, before ever shelling out — never merely hoping the ambient
// host process happens to carry it — so a real `sbx env create` observes it
// genuinely defined regardless of what the daemon's own environment holds.
const interpolationDefinedHostVar = "PIX_UAT_STORY0_DEFINED"

// interpolationDefinedHostValue is the exact known value
// interpolationDefinedHostVar is set to for the defined/default fixture's
// create call, asserted verbatim against that fixture's own exec probe
// output.
const interpolationDefinedHostValue = "pix-uat-story0-defined-value"

// interpolationMissingHostVar is the exact host environment variable name
// BOTH interpolation fixtures reference as an undefined variable:
// interpDefinedDefaultFixtureYAML's own missing-with-default `${VAR:-default}`
// form (AC-7's second required case), and interpMissingFixtureYAML's own
// bare `${VAR}` form (AC-7's third). The interpolation observation phase
// explicitly STRIPS this key from every create call's own env — never
// merely assuming the ambient host process happens to lack it — so a real
// `sbx env create` observes it genuinely undefined even if the host daemon
// process happens to carry it.
const interpolationMissingHostVar = "PIX_UAT_STORY0_MISSING"

// interpolationDefaultFallbackValue is the exact literal default
// interpDefinedDefaultFixtureYAML's own `${VAR:-default}` form declares.
const interpolationDefaultFallbackValue = "fallback-value"

// interpolationDefinedEnvKey / interpolationDefaultEnvKey are the exact
// sandbox-side environment variable names interpDefinedDefaultFixtureYAML
// assigns its two interpolated values to, so that fixture's own exec probe
// can read them back in an unambiguous, labeled line format.
const interpolationDefinedEnvKey = "PIX_UAT_INTERP_DEFINED"
const interpolationDefaultEnvKey = "PIX_UAT_INTERP_DEFAULT"

// interpolationMissingEnvKey is the exact sandbox-side environment variable
// name interpMissingFixtureYAML assigns its bare, undefined-variable
// interpolation to.
const interpolationMissingEnvKey = "PIX_UAT_INTERP_MISSING"

// interpDefinedDefaultFixtureName is the literal `pix-uatenv-*` sandbox name
// the defined/default interpolation fixture creates as — owned directly
// here, exactly like every other fixture name in this package.
const interpDefinedDefaultFixtureName = "pix-uatenv-interp-defined-default"

// interpMissingFixtureName is the literal `pix-uatenv-*` sandbox name the
// bare-missing-variable interpolation fixture creates as.
const interpMissingFixtureName = "pix-uatenv-interp-missing"

// interpDefinedDefaultFixtureYAML renders AC-7's first two required
// interpolation cases in the SAME declared environment, since both facets
// share one Executor env setup (interpolationDefinedHostVar explicitly set,
// interpolationMissingHostVar explicitly stripped): a plain defined `${VAR}`
// reference, and a missing-with-default `${VAR:-default}` reference. Like
// every other fixture in this file, it is a package-owned literal, never
// derived from envinfo.
func interpDefinedDefaultFixtureYAML() []byte {
	return []byte(`schemaVersion: "1"
agent: pix
name: ` + interpDefinedDefaultFixtureName + `

kits:
  - ./kit

env:
  ` + interpolationDefinedEnvKey + `: "${` + interpolationDefinedHostVar + `}"
  ` + interpolationDefaultEnvKey + `: "${` + interpolationMissingHostVar + `:-` + interpolationDefaultFallbackValue + `}"
`)
}

// interpDefinedDefaultFixture is the typed EnvironmentFixture
// interpDefinedDefaultFixtureYAML materializes through writeAuthoredFixture,
// exactly like every other `agent: pix` fixture in this package.
func interpDefinedDefaultFixture() EnvironmentFixture {
	return EnvironmentFixture{
		Name:         interpDefinedDefaultFixtureName,
		YAML:         interpDefinedDefaultFixtureYAML(),
		RelativeKits: []string{"./kit"},
	}
}

// interpMissingFixtureYAML renders AC-7's third required interpolation
// case: a bare `${VAR}` reference to interpolationMissingHostVar with no
// default at all — the case where BOTH a loader/create refusal and a
// create success (with the reference resolving to some observable
// sandbox-side value) are legitimate observation evidence
// (check_create_exec_interpolation.go).
func interpMissingFixtureYAML() []byte {
	return []byte(`schemaVersion: "1"
agent: pix
name: ` + interpMissingFixtureName + `

kits:
  - ./kit

env:
  ` + interpolationMissingEnvKey + `: "${` + interpolationMissingHostVar + `}"
`)
}

// interpMissingFixture is the typed EnvironmentFixture interpMissingFixtureYAML
// materializes through writeAuthoredFixture, exactly like every other
// `agent: pix` fixture in this package.
func interpMissingFixture() EnvironmentFixture {
	return EnvironmentFixture{
		Name:         interpMissingFixtureName,
		YAML:         interpMissingFixtureYAML(),
		RelativeKits: []string{"./kit"},
	}
}

// recreateBoundaryFixtureName is the literal pix-* sandbox name Story 0
// authors for environment_recreate_boundary's baseline declaration — owned
// directly here, exactly like the other two fixtures' names (fixtures.go's
// package doc and Name field explain why Story 0 owns naming directly).
const recreateBoundaryFixtureName = "pix-uatenv-fixture-recreate"

// recreateBoundaryEnvName is the literal environment name
// environment_recreate_boundary names in its recreate command. Story 0 never
// registers a named environment (Story 1's `pix env add` concern); this is a
// fixed literal this package owns outright, like every other fixture name in
// this file.
const recreateBoundaryEnvName = "recreate-fixture"

// recreateBoundaryMutatedFacet names the one representative effective
// declaration facet environment_recreate_boundary mutates between its
// baseline and drifted fixture. docs/design/environments.md section 9.1
// lists the creation fingerprint as covering "every effective create-time
// facet after upstream environment composition"; sandboxOptions.memory is
// one such facet, already present in customAgentFixture's sibling
// declaration, and picked here because it is scalar and unambiguous to diff.
// Any effective facet would exercise the same native contract.
const recreateBoundaryMutatedFacet = "sandboxOptions.memory"

// recreateBoundaryFixtureYAML renders the baseline declaration
// environment_recreate_boundary creates first. Like every other fixture in
// this file, it is a package-owned literal against the upstream schema,
// never derived from envinfo.
func recreateBoundaryFixtureYAML() []byte {
	return []byte(`schemaVersion: "1"
agent: pix
name: ` + recreateBoundaryFixtureName + `

kits:
  - ./kit

sandboxOptions:
  memory: 6g
`)
}

// recreateBoundaryMutatedFixtureYAML renders the SAME declared environment
// identity with only recreateBoundaryMutatedFacet changed — a representative
// effective-facet drift, not a full rewrite, so the check exercises exactly
// one changed fact between its two create calls.
func recreateBoundaryMutatedFixtureYAML() []byte {
	return []byte(`schemaVersion: "1"
agent: pix
name: ` + recreateBoundaryFixtureName + `

kits:
  - ./kit

sandboxOptions:
  memory: 60g
`)
}

// recreateBoundaryFixture is the typed EnvironmentFixture representation of
// recreateBoundaryFixtureYAML's baseline declaration, routed through
// writeAuthoredFixture exactly like every other `agent: pix` fixture in this
// package (customAgentFixture, ollamaCapabilityFixture, candidateImageFixture):
// its RelativeKits materializes a real `./kit` directory whose kit-spec
// declares agent identity "pix", closing the fresh UAT run
// run-20260824-095511-de9ece08 failure (`ERROR: "pix" is not a known agent`)
// the same way run-20260824-082317-e58d0587 was already closed for the other
// fixtures. The mutated fixture (recreateBoundaryMutatedFixtureYAML) declares
// the identical `kits:` entry and is written directly over this fixture's
// already-materialized YAML path, so the already-materialized `./kit` never
// needs to be re-created for the one changed facet
// (recreateBoundaryMutatedFacet).
func recreateBoundaryFixture() EnvironmentFixture {
	return EnvironmentFixture{
		Name:         recreateBoundaryFixtureName,
		YAML:         recreateBoundaryFixtureYAML(),
		RelativeKits: []string{"./kit"},
	}
}

// ollamaCapabilityFixtureName is the literal `pix-*` sandbox name Story 0
// authors for environment_custom_agent_ollama's declaration — owned
// directly here, exactly like every other fixture name in this package
// (this file's package doc).
const ollamaCapabilityFixtureName = "pix-uatenv-fixture-ollama"

// ollamaCapabilityMarker is the literal env key ollamaCapabilityFixtureYAML
// sets so a fake executor (or a human reading a captured artifact) can
// distinguish this fixture's create call from customAgentFixture's, whose
// YAML is otherwise structurally identical (`agent: pix` plus one kit path).
const ollamaCapabilityMarker = "PIX_OLLAMA_PROBE"

// ollamaCapabilityFixtureYAML renders the one authored declaration
// environment_custom_agent_ollama creates: the same custom Pix agent
// selection customAgentFixture proves (`agent: pix` plus one kit), with no
// model/resume facts of its own — this check probes sbx's own `--model`/
// `--provider` transport flags, not pi's session model. Like every other
// fixture in this file, it is a package-owned literal, never derived from
// envinfo.
func ollamaCapabilityFixtureYAML() []byte {
	return []byte(`schemaVersion: "1"
agent: pix
name: ` + ollamaCapabilityFixtureName + `

kits:
  - ./kit

env:
  ` + ollamaCapabilityMarker + `: "1"
`)
}

// ollamaCapabilityFixture is the one fixture
// checkEnvironmentCustomAgentOllama exercises: a registered environment
// whose `agent: pix` selects the candidate Pix custom agent kit, reusing
// customAgentFixture's Kit path since both checks target the same generated
// candidate kit layout.
func ollamaCapabilityFixture() EnvironmentFixture {
	return EnvironmentFixture{
		Name:         ollamaCapabilityFixtureName,
		YAML:         ollamaCapabilityFixtureYAML(),
		Kit:          "/opt/pix/kit",
		RelativeKits: []string{"./kit"},
	}
}

// customAgentFixture is the one fixture this unit's check exercises: a
// registered environment whose `agent: pix` selects the candidate Pix custom
// agent kit, with a deterministic set of live skill trees, model, and resume
// facts a name-based exec must reproduce exactly.
func customAgentFixture() EnvironmentFixture {
	const name = "pix-uatenv-fixture-0"
	yaml := `schemaVersion: "1"
agent: pix
name: ` + name + `

kits:
  - ./kit

sandboxOptions:
  memory: 16g

env:
  PIX_MEMORY_SCOPE: personal
`
	return EnvironmentFixture{
		Name:         name,
		YAML:         []byte(yaml),
		Kit:          "/opt/pix/kit",
		RelativeKits: []string{"./kit"},
		LiveSkills: []string{
			"/opt/pix/kit/skills",
			"/home/uat/personal-context/skills",
		},
		Model:  "anthropic/claude-sonnet-5",
		Resume: "session-fixture-1",
	}
}
