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
	// Name is the literal `pix-*` sandbox name this fixture's environment
	// creates as. Story 0 owns this name directly (a registered environment
	// normally omits `name`; Pix's own naming algorithm belongs to Story 1's
	// envinfo, not to this fixture).
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

sandboxOptions:
  memory: 60g
`)
}

// customAgentFixture is the one fixture this unit's check exercises: a
// registered environment whose `agent: pix` selects the candidate Pix custom
// agent kit, with a deterministic set of live skill trees, model, and resume
// facts a name-based exec must reproduce exactly.
func customAgentFixture() EnvironmentFixture {
	const yaml = `schemaVersion: "1"
agent: pix

kits:
  - ./kit

sandboxOptions:
  memory: 16g

env:
  PIX_MEMORY_SCOPE: personal
`
	return EnvironmentFixture{
		Name: "pix-uatenv-fixture-0",
		YAML: []byte(yaml),
		Kit:  "/opt/pix/kit",
		LiveSkills: []string{
			"/opt/pix/kit/skills",
			"/home/uat/personal-context/skills",
		},
		Model:  "anthropic/claude-sonnet-5",
		Resume: "session-fixture-1",
	}
}
