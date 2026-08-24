package uatenvmatrix

import (
	"fmt"
	"os"
	"path/filepath"
)

// fixtureKitSpecImage is the literal, hand-picked sandbox image this
// package's own materialized `./kit` fixtures pin — a pinned tag, never
// `:latest`, matching pi-kit/spec.yaml's own convention, but owned
// independently here (fixtures.go's package doc: this package's fixtures
// are literal bytes it owns outright, never derived from Pix's real kit).
// Like buildOllamaExecArgv's own flag-shape assumption, this is Story 0's
// own literal pending the same host-assumption review
// (docs/upstream/sbx-0.39-environments.md, Story 1's E0.7): it only has to
// be a real, pullable image so a live `sbx env create` can resolve and
// apply the kit, never a claim that a real candidate kit must use this
// exact tag.
const fixtureKitSpecImage = "docker.io/mcavage/pix:0.1.71"

// fixtureKitSpecYAML renders the minimal, self-contained kit-spec v2 body
// writeAuthoredFixture materializes at every relative kit path a fixture's
// authored YAML declares under `kits:`. It is deliberately the smallest
// shape the strict v2 grammar (schemaVersion "2", `permissions` not `caps`,
// list-valued `sandbox.entrypoint`) accepts: schemaVersion, kind, name, and
// one sandbox stanza with a pinned image and entrypoint — enough for a real
// `sbx env create` to resolve and apply the kit instead of failing on a
// missing directory, never a full copy of pi-kit/spec.yaml (this package
// must not derive its fixtures from Pix's real kit; matrix.go's package
// doc explains why).
func fixtureKitSpecYAML(kitName string) []byte {
	return []byte(fmt.Sprintf(`schemaVersion: "2"
kind: sandbox
name: %s
displayName: %s

sandbox:
  image: %q
  entrypoint: [pi]
`, kitName, kitName, fixtureKitSpecImage))
}

// writeAuthoredFixture writes fixture.YAML to phaseDir/filename and then
// materializes every path fixture.RelativeKits declares, as a real
// directory containing a minimal valid kit-spec v2 spec.yaml.
//
// Every relative path is resolved against phaseDir — the SAME directory the
// authored file itself lands in, since every check in this package writes
// its fixture directly beneath its own run-local phaseDir (matrix.go's
// matrixRoot) with no intermediate subdirectory — exactly how sbx itself
// resolves a relative `kits:` entry: relative to the `.sbxenv.yaml` file's
// own location, never the process's working directory. Host UAT run
// run-20260823-200503-777c37e1 hit `resolve kits: kit reference "./kit":
// path does not exist` because no check ever created that directory; every
// check that declares RelativeKits must route its fixture write through
// this one function so the fix cannot silently apply to one check while
// leaving a sibling fixture (e.g. environment_custom_agent_ollama's) with
// the identical bug under a different check name.
//
// This only ever creates local scratch files beneath the calling check's
// own phaseDir: it never touches the injected Executor, so it changes
// nothing about cleanup.go's receipt-gated remote-sandbox teardown (which
// governs the instance a create call may have produced, not this package's
// own local fixture bytes) or about run-local isolation (phaseDir was
// already scoped per run and per check before this function existed).
func writeAuthoredFixture(phaseDir, filename string, fixture EnvironmentFixture) (string, error) {
	fixturePath := filepath.Join(phaseDir, filename)
	if err := os.WriteFile(fixturePath, fixture.YAML, 0600); err != nil {
		return "", fmt.Errorf("write authored fixture: %w", err)
	}

	for _, rel := range fixture.RelativeKits {
		kitDir := filepath.Join(phaseDir, rel)
		if err := os.MkdirAll(kitDir, 0700); err != nil {
			return "", fmt.Errorf("materialize relative kit path %q: %w", rel, err)
		}
		specPath := filepath.Join(kitDir, "spec.yaml")
		if err := os.WriteFile(specPath, fixtureKitSpecYAML(filepath.Base(kitDir)), 0600); err != nil {
			return "", fmt.Errorf("materialize kit spec for %q: %w", rel, err)
		}
	}

	return fixturePath, nil
}
