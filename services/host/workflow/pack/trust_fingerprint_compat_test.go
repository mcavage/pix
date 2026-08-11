// trust_fingerprint_compat_test.go — the Tier-1 consent fingerprint's BACKWARD
// COMPATIBILITY, pinned structurally rather than by folklore.
//
// trust.go's fpDoc JSON encoding IS the acceptance identity: every already
// accepted pack is remembered by the hash of that document. Change a field name,
// a field ORDER, or an omitempty, and every existing user is dragged back
// through a scary "this pack wants to run things on your Mac" prompt for a
// surface they already approved — which trains people to say yes without
// reading, the single worst outcome this gate can have.
//
// The integrations remediation ADDED two fields to the setup section
// (Require/Apply, for the declarative setup form). They are `omitempty` on
// purpose, so a pack written in the older EXECUTABLE form (path + check_args +
// apply_args) must encode byte-identically to before and keep its acceptance.
//
// The test below does not take that on trust from a magic constant: it mirrors
// the PRE-REMEDIATION fpDoc structs — no Require/Apply fields at all — hashes
// that, and requires the live code to produce the same fingerprint. If a future
// edit renames a field, reorders one, bumps v, or drops an omitempty, this fails
// and names the re-gate it would have caused. (The two goldens in
// pack_services_test.go pin the same encoding from the other direction, by
// value, so the mirror below cannot silently drift along with production.)
package pack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"testing"
)

// --- the pre-remediation encoding, mirrored ---------------------------------
//
// These are DATA, not a second implementation: they are the wire format that
// shipped, frozen here so the live encoder can be compared against it.

type legacyFPProxy struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

type legacyFPBin struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
	Host bool   `json:"host"`
}

// legacyFPSetup is fpSetup WITHOUT Require/Apply — the shape a pack using the
// executable setup form encoded to before the declarative form existed.
type legacyFPSetup struct {
	ID          string   `json:"id"`
	Path        string   `json:"path"`
	SHA         string   `json:"sha"`
	CheckArgs   []string `json:"check_args"`
	ApplyArgs   []string `json:"apply_args"`
	Required    bool     `json:"required"`
	Description string   `json:"description"`
}

type legacyFPDoc struct {
	V             int                `json:"v"`
	MCP           []hostBoMMCP       `json:"mcp"`
	Containers    []hostBoMContainer `json:"container"`
	RemoteMCP     []hostBoMRemote    `json:"remote_mcp"`
	Proxies       []legacyFPProxy    `json:"proxy"`
	Bins          []legacyFPBin      `json:"bin"`
	Egress        []string           `json:"egress"`
	Creds         []string           `json:"cred"`
	Prerequisites []string           `json:"prerequisites"`
	Setup         []legacyFPSetup    `json:"setup"`
	Inference     []hostBoMInference `json:"inference"`
	Services      []packinfo.Service `json:"services,omitempty"`
}

// TestHostExecFingerprint_OldShapePackKeepsItsAcceptance is the load-bearing
// compatibility assertion: a pack whose setup is the OLD executable form and
// whose integrations are only remote/container (no new-model field used
// anywhere) fingerprints exactly as it did before the remediation, so its
// recorded acceptance still matches and nobody is re-prompted.
func TestHostExecFingerprint_OldShapePackKeepsItsAcceptance(t *testing.T) {
	root := t.TempDir()
	hook := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(filepath.Join(root, "setup-account"), hook, 0o755); err != nil {
		t.Fatal(err)
	}
	// A pack in the old shape: an image container, a remote endpoint, one
	// credential name, and an EXECUTABLE setup hook. Nothing here touches
	// `command`, `probe`, `require` or `apply`.
	p := &packinfo.Info{Root: root, Manifest: packinfo.Manifest{
		Name: "work",
		Integrations: []packinfo.Integration{
			{Name: "HR", MCP: "hr", Image: "hr-mcp:0.0.1", Env: "HR_API_KEY", EnvKeys: []string{"HR_DOMAIN"}},
			{Name: "Docs", MCP: "docs", URL: "https://docs.example.test/mcp"},
		},
		Setup: []packinfo.SetupStep{{
			ID: "account", Path: "setup-account", Description: "Connect the account",
			CheckArgs: []string{"check"}, ApplyArgs: []string{"apply"}, Required: true,
		}},
	}}
	bom := ComputeHostBoM(p)
	got, _, err := ComputeHostExecFingerprint(root, bom)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	sum := sha256.Sum256(hook)
	want := legacyFPDoc{
		V: 6,
		Containers: []hostBoMContainer{{
			Name: "hr", Image: "hr-mcp:0.0.1",
			// The declared secret first, then the pack's own list, each section
			// sorted canonically by the encoder.
			EnvKeys: []string{"HR_API_KEY", "HR_DOMAIN"},
		}},
		RemoteMCP: []hostBoMRemote{{Name: "docs", URL: "https://docs.example.test/mcp"}},
		Creds:     []string{"HR_API_KEY"},
		Setup: []legacyFPSetup{{
			ID: "account", Path: "setup-account", SHA: hex.EncodeToString(sum[:]),
			CheckArgs: []string{"check"}, ApplyArgs: []string{"apply"},
			Required: true, Description: "Connect the account",
		}},
	}
	enc, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	legacySum := sha256.Sum256(enc)
	if legacy := hex.EncodeToString(legacySum[:]); got != legacy {
		t.Fatalf("COMPATIBILITY BREAK: an old-shape pack's Tier-1 fingerprint changed.\n"+
			" got  %s\n want %s (the pre-remediation encoding)\n"+
			"Every already-accepted pack would re-gate and every user would be re-prompted.\n"+
			"live encoding must equal: %s", got, legacy, enc)
	}
}

// TestHostExecFingerprint_SetupRequireApplyAreOmitEmpty is the mechanism behind
// the compatibility above, pinned on its own so a lost `omitempty` is diagnosed
// precisely rather than as a mystery hash change: for a step that uses neither
// field, nil and EMPTY-slice values must encode identically (they differ as
// `null` vs `[]` the moment omitempty is dropped) — and a step that DOES use
// them must fingerprint differently, because a declarative step names binaries
// and argv that run on this host and therefore has to be consented to.
func TestHostExecFingerprint_SetupRequireApplyAreOmitEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hook"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	fpOf := func(step packinfo.SetupStep) string {
		t.Helper()
		fp, _, err := ComputeHostExecFingerprint(root, hostBoM{Setup: []packinfo.SetupStep{step}})
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return fp
	}
	executable := packinfo.SetupStep{ID: "account", Path: "hook", CheckArgs: []string{"check"}, ApplyArgs: []string{"apply"}}
	base := fpOf(executable)

	emptyNotNil := executable
	emptyNotNil.Require = []packinfo.SetupRequire{}
	emptyNotNil.Apply = []packinfo.SetupApply{}
	if fpOf(emptyNotNil) != base {
		t.Error("Require/Apply lost their omitempty: an empty value now encodes as [] instead of being omitted, " +
			"which re-gates every accepted pack")
	}

	declarative := packinfo.SetupStep{ID: "account",
		Require: []packinfo.SetupRequire{{Kind: "bin", Name: "gog", Install: "brew install gog"}},
		Apply:   []packinfo.SetupApply{{Kind: "interactive", Argv: []string{"gog", "auth", "login"}}}}
	if fpOf(declarative) == base {
		t.Error("a declarative step is EXECUTABLE INTENT (it names binaries and argv that run on this host) " +
			"and must be covered by the fingerprint")
	}
	// And a change WITHIN the declarative data re-gates too.
	changed := declarative
	changed.Apply = []packinfo.SetupApply{{Kind: "interactive", Argv: []string{"gog", "auth", "login", "--force"}}}
	if fpOf(changed) == fpOf(declarative) {
		t.Error("changing a declarative apply's argv must change the fingerprint")
	}
}
