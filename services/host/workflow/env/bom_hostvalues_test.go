package env

import "testing"

// bom_hostvalues_test.go proves BillOfMaterials.Values: computeHostValues
// derives it correctly from a pix.toml `[host.values.<NAME>]` table,
// ValueMeta finds it by name, and — the actual point of the type — editing
// its presentation fields (label/help/example) never moves the
// environment's trust fingerprint, while its one behavioral field
// (Required) is available with EffectiveRequired's default already
// applied.

const hostValuesSidecar = `schema = 1

[host.mcp.google-workspace]
env_keys = ["GOG_KEYRING_PASSWORD"]
plain_keys = ["GOG_ACCOUNT"]

[host.values.GOG_ACCOUNT]
label = "Google Workspace account email"
help = "The Google Workspace user gog authenticates as."
example = "you@company.com"
required = false

[host.values.GOG_KEYRING_PASSWORD]
label = "Google Workspace keyring password"
`

func TestComputeBoM_HostValuesPopulatedFromSidecar(t *testing.T) {
	env := loadTestEnv(t, "work", minimalSbxenv, hostValuesSidecar)
	bom, err := ComputeBoM(env, nil, nil)
	if err != nil {
		t.Fatalf("ComputeBoM: %v", err)
	}

	acct, ok := bom.ValueMeta("GOG_ACCOUNT")
	if !ok {
		t.Fatal("ValueMeta(GOG_ACCOUNT) not found")
	}
	if acct.Label != "Google Workspace account email" {
		t.Errorf("Label = %q", acct.Label)
	}
	if acct.Help != "The Google Workspace user gog authenticates as." {
		t.Errorf("Help = %q", acct.Help)
	}
	if acct.Example != "you@company.com" {
		t.Errorf("Example = %q", acct.Example)
	}
	if acct.Required {
		t.Error("Required = true, want false (authored required = false)")
	}

	pw, ok := bom.ValueMeta("GOG_KEYRING_PASSWORD")
	if !ok {
		t.Fatal("ValueMeta(GOG_KEYRING_PASSWORD) not found")
	}
	if !pw.Required {
		t.Error("Required = false, want true (default when `required` was never authored)")
	}

	if _, ok := bom.ValueMeta("NEVER_DECLARED"); ok {
		t.Error("ValueMeta must return ok=false for a name with no [host.values] entry")
	}
}

// TestFingerprint_HostValuesPresentationFieldsDoNotChangeFingerprint proves
// the deliberate exclusion documented on HostValueFact: rewording a
// setup-prompt Label/Help/Example is not host execution, a credential
// handoff, or a mount expansion, so it must never re-gate an otherwise
// unchanged environment.
func TestFingerprint_HostValuesPresentationFieldsDoNotChangeFingerprint(t *testing.T) {
	env := loadTestEnv(t, "work", minimalSbxenv, hostValuesSidecar)
	base, err := ComputeBoM(env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	baseFP, err := Fingerprint(base)
	if err != nil {
		t.Fatal(err)
	}

	reworded := base
	reworded.Values = append([]HostValueFact(nil), base.Values...)
	for i := range reworded.Values {
		if reworded.Values[i].Name == "GOG_ACCOUNT" {
			reworded.Values[i].Label = "Something else entirely"
			reworded.Values[i].Help = "Completely different help text."
			reworded.Values[i].Example = "nobody@example.org"
			reworded.Values[i].Required = !reworded.Values[i].Required
		}
	}
	rewordedFP, err := Fingerprint(reworded)
	if err != nil {
		t.Fatal(err)
	}
	if rewordedFP != baseFP {
		t.Errorf("Fingerprint changed after editing only Values presentation fields: base=%s reworded=%s", baseFP, rewordedFP)
	}
}

// TestComputeBoM_NoHostValuesTableLeavesValuesEmpty proves an environment
// that declares no [host.values] table at all gets a nil/empty Values
// slice, and ValueMeta reports ok=false for every name — the exact
// pre-existing behavior every declared env_keys/plain_keys name had before
// this type existed.
func TestComputeBoM_NoHostValuesTableLeavesValuesEmpty(t *testing.T) {
	env := loadTestEnv(t, "work", minimalSbxenv, `schema = 1

[host.mcp.google-workspace]
plain_keys = ["GOG_ACCOUNT"]
`)
	bom, err := ComputeBoM(env, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(bom.Values) != 0 {
		t.Errorf("Values = %+v, want empty", bom.Values)
	}
	if _, ok := bom.ValueMeta("GOG_ACCOUNT"); ok {
		t.Error("ValueMeta(GOG_ACCOUNT) = ok, want false with no [host.values] table authored")
	}
}
