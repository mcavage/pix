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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"pix/host/packinfo"
	"strings"
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

// TestHostExecFingerprint_ProbeIsExecutableIntent closes a gap an independent
// reviewer found in the first cut of this change: a pack's health `probe` is
// EXECUTED on the host by `pix doctor`, but it was neither fingerprinted nor
// rendered on the Tier-1 consent screen.
//
// That combination is the bad one. A pack could change a command pix runs on
// your machine, and you would neither have seen the original when you consented
// nor be re-asked when it changed. A probe is executable intent, exactly like a
// setup apply, on every transport that can carry one.
func TestHostExecFingerprint_ProbeIsExecutableIntent(t *testing.T) {
	root := t.TempDir()
	fpOf := func(b hostBoM) string {
		t.Helper()
		fp, _, err := ComputeHostExecFingerprint(root, b)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		return fp
	}
	for _, tc := range []struct {
		name         string
		without, wit hostBoM
	}{
		{
			name:    "command",
			without: hostBoM{MCP: []hostBoMMCP{{Name: "gw", Argv: []string{"gog", "mcp"}}}},
			wit:     hostBoM{MCP: []hostBoMMCP{{Name: "gw", Argv: []string{"gog", "mcp"}, Probe: []string{"curl", "http://evil"}}}},
		},
		{
			name:    "container",
			without: hostBoM{Containers: []hostBoMContainer{{Name: "hr", Image: "hr:1"}}},
			wit:     hostBoM{Containers: []hostBoMContainer{{Name: "hr", Image: "hr:1", Probe: []string{"curl", "http://evil"}}}},
		},
		{
			name:    "remote",
			without: hostBoM{RemoteMCP: []hostBoMRemote{{Name: "crm", URL: "https://x/mcp"}}},
			wit:     hostBoM{RemoteMCP: []hostBoMRemote{{Name: "crm", URL: "https://x/mcp", Probe: []string{"curl", "http://evil"}}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if fpOf(tc.without) == fpOf(tc.wit) {
				t.Errorf("adding a %s probe did not change the fingerprint: a pack could introduce a "+
					"command that `pix doctor` runs on this host without re-gating", tc.name)
			}
			// omitempty must hold, or every already-accepted pack re-gates.
			nilVsEmpty := tc.without
			switch tc.name {
			case "command":
				nilVsEmpty.MCP[0].Probe = []string{}
			case "container":
				nilVsEmpty.Containers[0].Probe = []string{}
			case "remote":
				nilVsEmpty.RemoteMCP[0].Probe = []string{}
			}
			if fpOf(nilVsEmpty) != fpOf(tc.without) {
				t.Errorf("%s probe lost omitempty: an empty value encodes as [] instead of being "+
					"omitted, which re-gates every accepted pack", tc.name)
			}
		})
	}
}

// TestRenderHostBoM_ShowsEveryCommandThatRuns: the consent screen must print
// the REAL command. Two ways it lied in the first cut — an unconditional
// `op run --` prefix on a server that declares no credentials and is therefore
// never wrapped, and a probe that was executed but never shown.
func TestRenderHostBoM_ShowsEveryCommandThatRuns(t *testing.T) {
	var buf bytes.Buffer
	renderHostBoM(&buf, hostBoM{MCP: []hostBoMMCP{
		{Name: "credfree", Argv: []string{"pio", "serve"}},
		{Name: "credful", Argv: []string{"gog", "mcp"}, EnvKeys: []string{"GOG_KEYRING_PASSWORD"}, Probe: []string{"gog", "auth", "doctor"}},
	}})
	out := buf.String()
	if strings.Contains(out, "op run -- pio serve") {
		t.Errorf("a credential-free server is never op-run wrapped; the screen must not claim it is:\n%s", out)
	}
	if !strings.Contains(out, "Runs on this Mac: pio serve") {
		t.Errorf("the bare command must be shown verbatim:\n%s", out)
	}
	if !strings.Contains(out, "op run -- gog mcp") {
		t.Errorf("a credentialed server IS wrapped and the screen must show that:\n%s", out)
	}
	if !strings.Contains(out, "GOG_KEYRING_PASSWORD") {
		t.Errorf("which credentials a host command receives is part of what you consent to:\n%s", out)
	}
	if !strings.Contains(out, "gog auth doctor") {
		t.Errorf("`pix doctor` executes the probe on this host, so it must appear on the screen:\n%s", out)
	}
}
