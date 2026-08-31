package release

import (
	"strings"
	"testing"
)

func validManifest() Manifest {
	return Manifest{
		Version:         "2.0.0",
		PixAgentDigest:  "sha256:" + strings.Repeat("a", 64),
		PixMemoryDigest: "sha256:" + strings.Repeat("b", 64),
		RuntimeDigest:   "sha256:" + strings.Repeat("c", 64),
		KitRevision:     "kit-rev-1",
	}
}

func TestManifest_Validate_Valid(t *testing.T) {
	if err := validManifest().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil for a well-formed manifest", err)
	}
}

func TestManifest_Validate_ReportsEveryProblem(t *testing.T) {
	m := Manifest{} // every field empty
	err := m.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want an error for an empty manifest")
	}
	for _, want := range []string{"version", "pix_agent_digest", "pix_memory_digest", "runtime_digest", "kit_revision"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error = %q, want it to mention %q", err, want)
		}
	}
}

func TestManifest_Validate_RejectsMutableTag(t *testing.T) {
	m := validManifest()
	m.PixAgentDigest = "docker.io/mcavage/pix-agent:latest"
	err := m.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want a convenience tag refused as launch identity")
	}
	if !strings.Contains(err.Error(), "pix_agent_digest") {
		t.Errorf("Validate() error = %q, want it to name pix_agent_digest", err)
	}
}

// TestManifest_Validate_RejectsUnsafeVersion is the defense-in-depth
// finding: Version used to be checked ONLY for non-empty, yet it flows
// unquoted into a filesystem path (RuntimeDir joins it under "runtime/",
// extractRuntimeArchive names a tar-entry prefix from it). Every one of
// these must be refused by the safe filename/release grammar.
func TestManifest_Validate_RejectsUnsafeVersion(t *testing.T) {
	cases := []string{
		"../../../etc/passwd",
		"..",
		".",
		"1.0.0/../../evil",
		"1.0.0..",
		"1.0..0",
		"a/b",
		"a\\b",
		"/etc/passwd",
		"1.0.0\x00",
		"1.0.0\n",
		"-1.0.0", // must not begin with a non-alnum
		".1.0.0",
		"",
		" ",
	}
	for _, version := range cases {
		t.Run(version, func(t *testing.T) {
			m := validManifest()
			m.Version = version
			err := m.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil for version %q, want an error", version)
			}
			if !strings.Contains(err.Error(), "version") {
				t.Errorf("Validate() error = %q, want it to name version", err)
			}
		})
	}
}

// TestManifest_Validate_AcceptsSemverCompatibleVersions proves the grammar
// stays usable for every real release-version shape (plain semver, a
// prerelease/build suffix using the RFC-compatible '-'/'+'... wait '+' is
// not in the allowed set, so build metadata must use '-' or '.' instead)
// while still refusing a path.
func TestManifest_Validate_AcceptsSemverCompatibleVersions(t *testing.T) {
	for _, version := range []string{"2.0.0", "2.0.0-beta.1", "2.0.0-rc1", "v2", "20240101"} {
		t.Run(version, func(t *testing.T) {
			m := validManifest()
			m.Version = version
			if err := m.Validate(); err != nil {
				t.Errorf("Validate() = %v for version %q, want nil", err, version)
			}
		})
	}
}

func TestManifest_Validate_RejectsUppercaseOrShortDigest(t *testing.T) {
	cases := map[string]string{
		"uppercase hex": "sha256:" + strings.Repeat("A", 64),
		"too short":     "sha256:abc123",
		"missing algo":  strings.Repeat("a", 64),
		"empty":         "",
	}
	for name, digest := range cases {
		t.Run(name, func(t *testing.T) {
			m := validManifest()
			m.RuntimeDigest = digest
			if err := m.Validate(); err == nil {
				t.Errorf("Validate() = nil for RuntimeDigest %q, want an error", digest)
			}
		})
	}
}

func TestParse_RoundTrip(t *testing.T) {
	m := validManifest()
	data, err := m.Encode()
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if *got != m {
		t.Errorf("Parse(Encode(m)) = %+v, want %+v", *got, m)
	}
}

func TestParse_RejectsUnknownField(t *testing.T) {
	data := []byte(`{
		"version": "2.0.0",
		"pix_agent_digest": "sha256:` + strings.Repeat("a", 64) + `",
		"pix_memory_digest": "sha256:` + strings.Repeat("b", 64) + `",
		"runtime_digest": "sha256:` + strings.Repeat("c", 64) + `",
		"kit_revision": "kit-rev-1",
		"price": "free"
	}`)
	if _, err := Parse(data); err == nil {
		t.Fatal("Parse() = nil error, want an unrecognized field refused")
	}
}

func TestParse_RejectsInvalidManifest(t *testing.T) {
	data := []byte(`{"version": "2.0.0"}`)
	if _, err := Parse(data); err == nil {
		t.Fatal("Parse() = nil error, want missing required fields refused")
	}
}

func TestParse_RejectsMalformedJSON(t *testing.T) {
	if _, err := Parse([]byte("not json")); err == nil {
		t.Fatal("Parse() = nil error, want malformed JSON refused")
	}
}

func TestEncode_RefusesInvalidManifest(t *testing.T) {
	if _, err := (Manifest{}).Encode(); err == nil {
		t.Fatal("Encode() = nil error, want an invalid manifest refused before it can reach disk")
	}
}
