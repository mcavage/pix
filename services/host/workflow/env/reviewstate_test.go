package env

import (
	"testing"

	"pix/host/hosttrust"
)

// emptyTrustStore is a fresh, in-memory environmentTrustStore no test needs
// to persist to disk — computeReviewState only ever reads it.
func emptyTrustStore() *environmentTrustStore {
	return &environmentTrustStore{Version: 1}
}

// ── the four explicit states ──────────────────────────────────────────────

func TestComputeReviewState_Tier0IsNotRequired(t *testing.T) {
	env := loadMinimalEnv(t, minimalSbxenv, "")
	status, err := computeReviewState(env, emptyTrustStore(), nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ReviewNotRequired {
		t.Errorf("State = %q, want %q", status.State, ReviewNotRequired)
	}
	if status.Fingerprint != "" {
		t.Errorf("Fingerprint = %q, want empty for a Tier0 environment", status.Fingerprint)
	}
}

func TestComputeReviewState_NoRecordIsUnaccepted(t *testing.T) {
	sbxenv := "schemaVersion: \"1\"\nsecrets:\n  db:\n    command: [\"db-secret-tool\"]\n"
	env := loadMinimalEnv(t, sbxenv, "")
	status, err := computeReviewState(env, emptyTrustStore(), nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ReviewUnaccepted {
		t.Errorf("State = %q, want %q", status.State, ReviewUnaccepted)
	}
	if status.Fingerprint == "" {
		t.Error("Fingerprint = empty, want the computed fingerprint even though nothing was accepted")
	}
}

func TestComputeReviewState_MatchingRecordIsAccepted(t *testing.T) {
	sbxenv := "schemaVersion: \"1\"\nsecrets:\n  db:\n    command: [\"db-secret-tool\"]\n"
	env := loadMinimalEnv(t, sbxenv, "")
	bom, err := ComputeBoM(env, nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := Fingerprint(bom)
	if err != nil {
		t.Fatal(err)
	}
	ts := emptyTrustStore()
	ts.Put(env.Subject, hosttrust.Record{Fingerprint: fp})

	status, err := computeReviewState(env, ts, nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ReviewAccepted {
		t.Errorf("State = %q, want %q", status.State, ReviewAccepted)
	}
	if status.Fingerprint != fp {
		t.Errorf("Fingerprint = %q, want %q", status.Fingerprint, fp)
	}
}

func TestComputeReviewState_MismatchedRecordIsChanged(t *testing.T) {
	sbxenv := "schemaVersion: \"1\"\nsecrets:\n  db:\n    command: [\"db-secret-tool\"]\n"
	env := loadMinimalEnv(t, sbxenv, "")
	ts := emptyTrustStore()
	ts.Put(env.Subject, hosttrust.Record{Fingerprint: "stale-fingerprint-not-matching-anything"})

	status, err := computeReviewState(env, ts, nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != ReviewChanged {
		t.Errorf("State = %q, want %q", status.State, ReviewChanged)
	}
	if status.Fingerprint == "" || status.Fingerprint == "stale-fingerprint-not-matching-anything" {
		t.Errorf("Fingerprint = %q, want the FRESH computed fingerprint, not the stale record", status.Fingerprint)
	}
}

// BoM is carried on the result so a caller (show.go) never recomputes it a
// second time just to render facet counts alongside the state.
func TestComputeReviewState_CarriesTheComputedBoM(t *testing.T) {
	sbxenv := "schemaVersion: \"1\"\nmcp:\n  servers:\n    - name: worker-mcp\n      command: worker-mcp-server\n"
	env := loadMinimalEnv(t, sbxenv, "")
	status, err := computeReviewState(env, emptyTrustStore(), nil, noBareLookPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.BoM.MCPServers) != 1 {
		t.Errorf("BoM.MCPServers = %+v, want exactly one carried-through MCP server fact", status.BoM.MCPServers)
	}
}
