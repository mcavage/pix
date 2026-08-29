// attribution_test.go — E2.2's red-first proof for ComputeFingerprint +
// Attribute (AC-72, AC-25 data half): a creation fingerprint over every
// EFFECTIVE (post-composition) facet, comment/formatting-insensitive but
// canonical-value-sensitive, plus a drift attribution map back to E1.2's
// PRE-composition Tree — identity-bearing facets named by identity,
// identityless list changes collapsed to a count, never a hash-only
// message, and interpolation carrying only the authored expression plus an
// opaque keyed digest, never a raw resolved value or an unkeyed hash of
// one.
package envinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// richFixtureYAML exercises every facet ComputeFingerprint fingerprints:
// env (one interpolated, one plain), sandboxOptions, secrets, registries
// (with noVerify), bindings, two mcp servers, a port, and two kits — so a
// single fixture proves fingerprint coverage AND identity-vs-index
// attribution in one place.
const richFixtureYAML = `schemaVersion: "1"
agent: pix

kits:
  - ./kit-a
  - ./kit-b

sandboxOptions:
  memory: 16g

env:
  PLAIN: value
  SECRET_TOKEN: ${GH_TOKEN}

secrets:
  demo:
    ref: op://vault/demo

registries:
  ghcr.io:
    ref: op://vault/ghcr
    noVerify: true

bindings:
  openai:
    apiKey:
      domains: ["api.openai.com"]

mcp:
  servers:
    - name: serverA
      url: https://a.example.com

ports:
  - sandbox: 3000
    host: 3000
`

// richFixtureYAMLReformatted is byte-DIFFERENT (comments added, blank lines
// changed, key order in a flow-insensitive spot) but semantically IDENTICAL
// to richFixtureYAML — the fixture TestComputeFingerprint_
// CommentAndFormattingInsensitive proves produces the same fingerprint.
const richFixtureYAMLReformatted = `# a native environment
schemaVersion: "1"
agent: pix   # the pix agent

kits:
  - ./kit-a
  - ./kit-b

sandboxOptions:
  memory: 16g

# env vars
env:
  PLAIN: value
  SECRET_TOKEN: ${GH_TOKEN}

secrets:
  demo:
    ref: op://vault/demo

registries:
  ghcr.io:
    ref: op://vault/ghcr
    noVerify: true

bindings:
  openai:
    apiKey:
      domains: ["api.openai.com"]

mcp:
  servers:
    - name: serverA
      url: https://a.example.com

ports:
  - sandbox: 3000
    host: 3000
`

func mustParseMerged(t *testing.T, yamlSrc string) (*Document, *Merged, *Tree) {
	t.Helper()
	doc, err := ParseBytes([]byte(yamlSrc), "fixture.sbxenv.yaml", ".")
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	merged, err := Merge(doc)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	tree, err := BuildTree(merged)
	if err != nil {
		t.Fatalf("BuildTree: %v", err)
	}
	return doc, merged, tree
}

func fixtureFacts(t *testing.T, yamlSrc, sandboxName string) RuntimeFacts {
	t.Helper()
	doc, _, _ := mustParseMerged(t, yamlSrc)
	return RuntimeFacts{
		Document:    doc,
		SandboxName: sandboxName,
		Template:    "docker.io/mcavage/pix:v0.0.0",
		PullPolicy:  "missing",
		PrimaryWorkspace: WorkspaceFact{
			Path: "/home/user/work",
		},
	}
}

// stubResolver signs a fake "resolved value" for every ${VAR} with a
// caller-supplied digest function — modeling hosttrust.SignResolvedValue
// without this package importing hosttrust (an L1 sibling; see doc.go).
func stubResolver(digest func(varName string) (string, error)) InterpolationResolver {
	return func(varName string, def *string) (string, error) {
		return digest(varName)
	}
}

// ── comments/formatting vs. canonical values ────────────────────────────

func TestComputeFingerprint_CommentAndFormattingInsensitive(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "digest-abc", nil })

	fpA, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(original): %v", err)
	}
	fpB, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAMLReformatted, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(reformatted): %v", err)
	}
	if len(diffFingerprintKeys(fpA, fpB)) != 0 {
		t.Errorf("a comment/whitespace-only edit changed the fingerprint: diff = %v", diffFingerprintKeys(fpA, fpB))
	}
}

func TestComputeFingerprint_CanonicalValueChangeDetected(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "digest-abc", nil })
	changed := strings.Replace(richFixtureYAML, "value", "different-value", 1)

	fpA, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(original): %v", err)
	}
	fpB, err := ComputeFingerprint(fixtureFacts(t, changed, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(changed): %v", err)
	}
	diff := diffFingerprintKeys(fpA, fpB)
	if len(diff) == 0 {
		t.Fatal("a canonical value change produced no fingerprint diff at all")
	}
	found := false
	for _, k := range diff {
		if k == "env.PLAIN" {
			found = true
		}
	}
	if !found {
		t.Errorf("diff = %v, want env.PLAIN among the changed keys", diff)
	}
}

// ── interpolation: authored expression + keyed digest only ─────────────

func TestComputeFingerprint_InterpolationNeverCarriesRawValueOrUnkeyedHash(t *testing.T) {
	const rawResolvedValue = "s3cr3t-raw-github-pat-value"
	unkeyedHash := sha256.Sum256([]byte(rawResolvedValue))
	unkeyedHashHex := hex.EncodeToString(unkeyedHash[:])

	resolve := stubResolver(func(varName string) (string, error) {
		if varName != "GH_TOKEN" {
			t.Fatalf("resolver called for unexpected var %q", varName)
		}
		// The resolver is the ONLY place a raw value could exist, and it
		// returns an opaque, already-keyed digest — never the value, never
		// an unkeyed hash of it.
		return "keyed-digest-deadbeef", nil
	})

	fp, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint: %v", err)
	}
	v, ok := fp["env.SECRET_TOKEN"]
	if !ok {
		t.Fatal("fingerprint has no env.SECRET_TOKEN entry")
	}
	if !strings.Contains(v, "${GH_TOKEN}") {
		t.Errorf("env.SECRET_TOKEN fingerprint value %q must carry the authored expression", v)
	}
	if !strings.Contains(v, "keyed-digest-deadbeef") {
		t.Errorf("env.SECRET_TOKEN fingerprint value %q must carry the resolver's keyed digest", v)
	}
	for _, fv := range fp {
		if strings.Contains(fv, rawResolvedValue) {
			t.Fatalf("fingerprint value %q contains the RAW resolved value", fv)
		}
		if strings.Contains(fv, unkeyedHashHex) {
			t.Fatalf("fingerprint value %q contains an UNKEYED hash of the resolved value", fv)
		}
	}
}

func TestComputeFingerprint_ResolverErrorPropagatesHMACKeyMissing(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "", ErrHMACKeyMissing })
	_, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if !errors.Is(err, ErrHMACKeyMissing) {
		t.Fatalf("ComputeFingerprint error = %v, want errors.Is ErrHMACKeyMissing", err)
	}
}

// ── Attribute: identity-bearing, identityless-list, and F9 growth ──────

func TestAttribute_IdentityBearingFacetNamedByItsKeyPath(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "digest-abc", nil })
	stored, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(stored): %v", err)
	}
	changed := strings.Replace(richFixtureYAML, "value", "different-value", 1)
	currentFacts := fixtureFacts(t, changed, "pix-demo")
	current, err := ComputeFingerprint(currentFacts, resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(current): %v", err)
	}
	_, _, pre := mustParseMerged(t, changed)

	drifts := Attribute(pre, stored, current)
	if len(drifts) != 1 {
		t.Fatalf("Attribute = %+v, want exactly one drift", drifts)
	}
	d := drifts[0]
	if !d.Identity {
		t.Errorf("Identity = false, want true for env.PLAIN")
	}
	if d.KeyPath != "env.PLAIN" {
		t.Errorf("KeyPath = %q, want %q", d.KeyPath, "env.PLAIN")
	}
	if !strings.Contains(d.Message, "env.PLAIN") {
		t.Errorf("Message = %q, must name the pre-composition key path", d.Message)
	}
}

// TestAttribute_IdentitylessListGrowthNeverMisattributesByIndex is E2.2's
// F9 case (mirroring tree_test.go's TestTree_MCPServerInsertionStability
// contrast): inserting an unrelated kit BEFORE an existing one shifts
// every existing kit's INDEX, which would misattribute drift to the wrong
// entry under naive per-index comparison. The identityless-list collapse
// must report one grouped drift instead of individually-named, silently
// wrong per-index attributions.
func TestAttribute_IdentitylessListGrowthNeverMisattributesByIndex(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "digest-abc", nil })
	stored, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(stored): %v", err)
	}
	grown := strings.Replace(richFixtureYAML, "kits:\n  - ./kit-a", "kits:\n  - ./kit-z\n  - ./kit-a", 1)
	currentFacts := fixtureFacts(t, grown, "pix-demo")
	current, err := ComputeFingerprint(currentFacts, resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(current): %v", err)
	}
	_, _, pre := mustParseMerged(t, grown)

	drifts := Attribute(pre, stored, current)
	if len(drifts) != 1 {
		t.Fatalf("Attribute = %+v, want exactly one collapsed drift for the identityless kits[] list", drifts)
	}
	d := drifts[0]
	if d.Identity {
		t.Errorf("Identity = true, want false for a collapsed identityless-list drift")
	}
	if d.ComposedKey != "kits[]" {
		t.Errorf("ComposedKey = %q, want %q", d.ComposedKey, "kits[]")
	}
	if d.EntriesChanged != 3 {
		t.Errorf("EntriesChanged = %d, want 3 (every index shifted by the insertion, plus the new one)", d.EntriesChanged)
	}
	if !strings.Contains(d.Message, "kits[] (3 entries changed)") {
		t.Errorf("Message = %q, want the literal collapsed count form", d.Message)
	}
}

// TestAttribute_UnrelatedIdentityNeverFlaggedByASiblingsInsertion is F9's
// identity-addressed CONTRAST: adding an unrelated mcp server must never
// cause the existing server's OWN identity-addressed key path to appear in
// the drift list at all.
func TestAttribute_UnrelatedIdentityNeverFlaggedByASiblingsInsertion(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "digest-abc", nil })
	stored, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(stored): %v", err)
	}
	withSibling := strings.Replace(richFixtureYAML,
		"    - name: serverA\n      url: https://a.example.com\n",
		"    - name: serverA\n      url: https://a.example.com\n    - name: serverB\n      url: https://b.example.com\n",
		1)
	currentFacts := fixtureFacts(t, withSibling, "pix-demo")
	current, err := ComputeFingerprint(currentFacts, resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(current): %v", err)
	}
	_, _, pre := mustParseMerged(t, withSibling)

	drifts := Attribute(pre, stored, current)
	for _, d := range drifts {
		if d.KeyPath == "mcp.servers[serverA]" {
			t.Fatalf("serverA's own key path must never appear in drift after only serverB was added; got %+v", drifts)
		}
	}
	foundServerB := false
	for _, d := range drifts {
		if d.KeyPath == "mcp.servers[serverB]" {
			foundServerB = true
			if !d.Identity {
				t.Errorf("serverB drift Identity = false, want true (identity-addressed)")
			}
		}
	}
	if !foundServerB {
		t.Errorf("drifts = %+v, want at least one entry attributed to mcp.servers[serverB]", drifts)
	}
}

// TestAttribute_PixManagedFacetWithNoPreCompositionSource covers a
// composed facet that traces to no authored key path at all (the pinned
// template, a Pix-owned singleton) — still identity-bearing (there is
// exactly one of it), never a hash-only message.
func TestAttribute_PixManagedFacetWithNoPreCompositionSource(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "digest-abc", nil })
	facts := fixtureFacts(t, richFixtureYAML, "pix-demo")
	stored, err := ComputeFingerprint(facts, resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(stored): %v", err)
	}
	facts.Template = "docker.io/mcavage/pix:v0.0.1"
	current, err := ComputeFingerprint(facts, resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(current): %v", err)
	}
	_, _, pre := mustParseMerged(t, richFixtureYAML)

	drifts := Attribute(pre, stored, current)
	if len(drifts) != 1 {
		t.Fatalf("Attribute = %+v, want exactly one drift", drifts)
	}
	d := drifts[0]
	if !d.Identity {
		t.Errorf("Identity = false, want true for a Pix-managed singleton")
	}
	if d.KeyPath != "" {
		t.Errorf("KeyPath = %q, want empty (no pre-composition source)", d.KeyPath)
	}
	if !strings.Contains(d.Message, "pix-managed") {
		t.Errorf("Message = %q, want it to name this as pix-managed", d.Message)
	}
}

// ── never a hash-only message ────────────────────────────────────────────

var bareHashRE = regexp.MustCompile(`^[0-9a-f]{16,}$`)

func TestAttribute_NoMessageIsEverHashOnly(t *testing.T) {
	resolve := stubResolver(func(string) (string, error) { return "digest-abc", nil })
	stored, err := ComputeFingerprint(fixtureFacts(t, richFixtureYAML, "pix-demo"), resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(stored): %v", err)
	}
	mutated := richFixtureYAML
	mutated = strings.Replace(mutated, "value", "different-value", 1)
	mutated = strings.Replace(mutated, "16g", "32g", 1)
	mutated = strings.Replace(mutated, "kits:\n  - ./kit-a", "kits:\n  - ./kit-z\n  - ./kit-a", 1)
	currentFacts := fixtureFacts(t, mutated, "pix-demo")
	current, err := ComputeFingerprint(currentFacts, resolve)
	if err != nil {
		t.Fatalf("ComputeFingerprint(current): %v", err)
	}
	_, _, pre := mustParseMerged(t, mutated)

	drifts := Attribute(pre, stored, current)
	if len(drifts) == 0 {
		t.Fatal("expected at least one drift from a multi-facet mutation")
	}
	for _, d := range drifts {
		if bareHashRE.MatchString(strings.TrimSpace(d.Message)) {
			t.Errorf("Drift.Message %q is hash-only, never allowed", d.Message)
		}
		if d.Message == "" {
			t.Error("Drift.Message must never be empty")
		}
	}
}

// ── reset invalidation: one record, not a flood ─────────────────────────

func TestResetInvalidatedDrift_IsExactlyOneRecord(t *testing.T) {
	d := ResetInvalidatedDrift()
	if d.Message != "acceptance invalidated by reset; recreate required" {
		t.Errorf("Message = %q, want the exact literal drift text", d.Message)
	}
	if !d.Identity {
		t.Errorf("Identity = false, want true (a single, unambiguous whole-environment record)")
	}
}
