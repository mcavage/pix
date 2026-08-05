// truststore_legacy_migration_test.go — the read-time backward-compat bridge
// for a pack-trust.json written before the dual-field collapse (W3/U08b):
// migrateLegacyActivation (truststore.go) imports a legacy single
// `"activation"` object into the `"activations"` ledger on load, skipping a
// duplicate, and Save never writes the legacy key back. Every test here
// writes a REAL temp pack-trust.json to disk and drives it through
// loadPackTrustStore/Save — no in-memory struct shortcuts — because the bug
// class this guards against is exactly "the wire format on an existing
// user's disk doesn't decode the way the in-memory struct assumes".
package pack

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLegacyStore drops raw JSON at the isolated host's trust-store path.
func writeLegacyStore(t *testing.T, raw string) {
	t.Helper()
	if err := os.WriteFile(packTrustStorePath(), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLoadPackTrustStore_LegacyActivationMigration table-drives the four
// shapes migrateLegacyActivation must handle at load: the solo pre-packslim
// case (only `activation`, no ledger), a duplicate of an already-ledgered
// pack (existing entry wins, no dup), a legacy entry for a DIFFERENT pack
// than the ledger (appended, order preserved), and a malformed `activation`
// value (ignored, rest of the store still parses). Each case writes a REAL
// pack-trust.json and drives it through loadPackTrustStore — no in-memory
// struct shortcuts.
func TestLoadPackTrustStore_LegacyActivationMigration(t *testing.T) {
	cases := []struct {
		name  string
		raw   string
		check func(t *testing.T, s *PackTrustStore)
	}{
		{
			name: "solo activation migrates into the ledger",
			raw: `{
				"version": 1,
				"activation": {
					"owner": "path:/packs/solo",
					"path": "/packs/solo",
					"mcp": ["slack"],
					"gog_account": "me@example.com",
					"prior_gog_account": ""
				}
			}`,
			check: func(t *testing.T, s *PackTrustStore) {
				if len(s.Activations) != 1 {
					t.Fatalf("want 1 migrated activation, got %d (%+v)", len(s.Activations), s.Activations)
				}
				got := s.Activations[0]
				if got.Owner != "path:/packs/solo" || got.Path != "/packs/solo" {
					t.Errorf("migrated record has wrong identity: %+v", got)
				}
				if len(got.MCP) != 1 || got.MCP[0] != "slack" {
					t.Errorf("migrated record dropped MCP attribution: %+v", got)
				}
				if got.GogAccount != "me@example.com" {
					t.Errorf("migrated record dropped gog_account attribution: %+v", got)
				}
				// activationFor must resolve through the migrated record — the
				// actual behavior the migration exists to preserve:
				// revertPackPriorContribution still finds this pack's
				// attribution after the upgrade.
				lock := s.activationFor("/packs/solo")
				if len(lock.MCP) != 1 || lock.MCP[0] != "slack" {
					t.Errorf("activationFor did not resolve through the migrated ledger entry: %+v", lock)
				}
			},
		},
		{
			name: "duplicate of an already-ledgered pack is deduped",
			raw: `{
				"version": 1,
				"activation": {
					"owner": "path:/packs/dup",
					"path": "/packs/dup",
					"mcp": ["stale-legacy-value"]
				},
				"activations": [
					{
						"owner": "path:/packs/dup",
						"path": "/packs/dup",
						"mcp": ["current-ledger-value"]
					}
				]
			}`,
			check: func(t *testing.T, s *PackTrustStore) {
				if len(s.Activations) != 1 {
					t.Fatalf("legacy activation for an already-ledgered pack must not duplicate; got %d entries: %+v", len(s.Activations), s.Activations)
				}
				if s.Activations[0].MCP[0] != "current-ledger-value" {
					t.Errorf("the existing ledger entry must win over the stale legacy value, got %+v", s.Activations[0])
				}
			},
		},
		{
			name: "legacy entry for a distinct pack is appended, order preserved",
			raw: `{
				"version": 1,
				"activation": {
					"owner": "path:/packs/legacy-solo",
					"path": "/packs/legacy-solo",
					"mcp": ["legacy-mcp"]
				},
				"activations": [
					{
						"owner": "path:/packs/stack-a",
						"path": "/packs/stack-a",
						"mcp": ["a-mcp"]
					}
				]
			}`,
			check: func(t *testing.T, s *PackTrustStore) {
				if len(s.Activations) != 2 {
					t.Fatalf("want the existing ledger entry preserved plus the migrated one, got %d: %+v", len(s.Activations), s.Activations)
				}
				if s.Activations[0].Owner != "path:/packs/stack-a" {
					t.Errorf("existing ledger order must be preserved, got %+v", s.Activations[0])
				}
				if s.Activations[1].Owner != "path:/packs/legacy-solo" || s.Activations[1].MCP[0] != "legacy-mcp" {
					t.Errorf("legacy activation for a distinct pack must be appended, got %+v", s.Activations[1])
				}
			},
		},
		{
			name: "malformed activation value is ignored, rest of store parses",
			raw: `{
				"version": 1,
				"activation": "not-an-object",
				"accepted": {"path:/x": {"fingerprint": "abcd"}}
			}`,
			check: func(t *testing.T, s *PackTrustStore) {
				if len(s.Activations) != 0 {
					t.Errorf("a malformed legacy activation must migrate nothing, got %+v", s.Activations)
				}
				if fp, ok := s.acceptedFingerprint("path:/x"); !ok || fp != "abcd" {
					t.Errorf("the rest of the store must still parse: acceptedFingerprint=%q ok=%v", fp, ok)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolatePackHost(t)
			writeLegacyStore(t, tc.raw)
			s, err := loadPackTrustStore()
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			tc.check(t, s)
		})
	}
}

// TestLoadPackTrustStore_MalformedStoreStillFailsClosed: an actually
// unparsable pack-trust.json (broken top-level JSON, not just a malformed
// nested key) must still hard-fail — migrateLegacyActivation runs strictly
// AFTER the primary unmarshal succeeds, so it must never mask a real parse
// failure into a silent empty store.
func TestLoadPackTrustStore_MalformedStoreStillFailsClosed(t *testing.T) {
	isolatePackHost(t)
	writeLegacyStore(t, `{not valid json at all`)

	if _, err := loadPackTrustStore(); err == nil {
		t.Error("a genuinely malformed store must fail closed, not silently produce an empty store")
	}
}

// TestLoadPackTrustStore_SymlinkedLegacyStoreStillRefused: the symlink
// refusal on read (round-2 finding F) must still gate BEFORE any legacy
// migration runs — a symlinked pack-trust.json pointing at an
// attacker-influenced file must never have its `activation` object migrated
// into the trusted ledger.
func TestLoadPackTrustStore_SymlinkedLegacyStoreStillRefused(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	crafted := filepath.Join(dir, "crafted.json")
	if err := os.WriteFile(crafted, []byte(`{"version":1,"activation":{"owner":"path:/evil","path":"/evil","mcp":["evil-mcp"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(crafted, packTrustStorePath()); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	s, err := loadPackTrustStore()
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("a symlinked trust store carrying a legacy activation must still be refused, got store=%+v err=%v", s, err)
	}
}

// TestLoadPackTrustStore_LegacyActivationRoundTrip: load-then-Save on a
// legacy-only store must produce a canonical file — `activations` populated,
// no `activation` key ever written back — and a SECOND load/Save cycle must
// be a no-op (no re-migration, no duplication): the migration is exactly
// once, at the first load of an old file.
func TestLoadPackTrustStore_LegacyActivationRoundTrip(t *testing.T) {
	isolatePackHost(t)
	writeLegacyStore(t, `{
		"version": 1,
		"activation": {
			"owner": "path:/packs/rt",
			"path": "/packs/rt",
			"mcp": ["rt-mcp"],
			"ollama_bridge_model": "qwen3.5:9b"
		}
	}`)

	s, err := loadPackTrustStore()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(s.Activations) != 1 {
		t.Fatalf("want 1 migrated activation before save, got %+v", s.Activations)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	onDisk, err := os.ReadFile(packTrustStorePath())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(onDisk, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["activation"]; ok {
		t.Errorf("Save must never write the legacy singular `activation` key back, got raw=%s", onDisk)
	}
	if _, ok := raw["activations"]; !ok {
		t.Errorf("Save must persist the canonical `activations` ledger, got raw=%s", onDisk)
	}

	// Second cycle: load the now-canonical file again and confirm no drift —
	// no re-migration path can fire (there is no `activation` key left to
	// find), so the ledger is exactly the one entry, unchanged.
	s2, err := loadPackTrustStore()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(s2.Activations) != 1 {
		t.Fatalf("reload of the canonical file must not duplicate, got %+v", s2.Activations)
	}
	if s2.Activations[0].Owner != "path:/packs/rt" || s2.Activations[0].OllamaBridgeModel != "qwen3.5:9b" {
		t.Errorf("round-tripped record lost data: %+v", s2.Activations[0])
	}
	if err := s2.Save(); err != nil {
		t.Fatalf("second save: %v", err)
	}
	finalRaw, err := os.ReadFile(packTrustStorePath())
	if err != nil {
		t.Fatal(err)
	}
	var finalDecoded map[string]any
	if err := json.Unmarshal(finalRaw, &finalDecoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := finalDecoded["activation"]; ok {
		t.Errorf("second save regressed to a non-canonical shape: %s", finalRaw)
	}
}
