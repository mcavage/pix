package config

import (
	"reflect"
	"testing"
)

// slp builds a non-nil *[]string for a profile override literal (an explicit
// REPLACE). slp() with no args yields a pointer to an empty slice — a
// present-empty override that disables an inherited list.
func slp(v ...string) *[]string {
	s := make([]string, 0, len(v))
	s = append(s, v...)
	return &s
}

// TestProfileGogOnlyInheritsRoundTrip is the omitempty gate: a profile that sets
// ONLY gog_account must Save+Load with its mcp/knowledge_bundles/kits still
// INHERITING (nil, not []). Without `,omitempty` on the Profile tags, Save would
// serialize the untouched nil slices as empty arrays, turning inherit into an
// explicit-empty REPLACE — this test fails if omitempty is removed.
func TestProfileGogOnlyInheritsRoundTrip(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", t.TempDir()+"/config.toml")
	c := &Config{MCP: []string{"gog", "slack"}}
	c.SetProfileGogAccount("work", "me@work.com")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got.Profiles["work"]
	if !ok {
		t.Fatalf("profile 'work' lost on round-trip: %+v", got.Profiles)
	}
	if p.GogAccount != "me@work.com" {
		t.Errorf("work.gog_account = %q, want me@work.com", p.GogAccount)
	}
	if p.MCP != nil {
		t.Errorf("work.mcp = %v, want nil (inherit) — pointer/omitempty regressed?", p.MCP)
	}
	if p.KnowledgeBundles != nil {
		t.Errorf("work.knowledge_bundles = %v, want nil (inherit)", p.KnowledgeBundles)
	}
	if p.Kits.Stack != nil {
		t.Errorf("work.kits.stack = %v, want nil (inherit)", p.Kits.Stack)
	}
	// And the inherit is real: the resolved profile sees the base mcp list.
	if !reflect.DeepEqual(got.Resolve("work").MCP, []string{"gog", "slack"}) {
		t.Errorf("Resolve(work).MCP = %v, want inherited [gog slack]", got.Resolve("work").MCP)
	}
}

// TestRemoveProfileMCPMaterializesInherit gates the effective-list mutators:
// unsetting an INHERITED value on a profile that has no prior mcp override must
// materialize base-minus-value as the profile's explicit list.
func TestRemoveProfileMCPMaterializesInherit(t *testing.T) {
	c := &Config{MCP: []string{"gog", "slack"}}
	if !c.RemoveProfileMCP("work", "slack") {
		t.Fatal("RemoveProfileMCP should report a change when removing an inherited value")
	}
	if mcp := c.Profiles["work"].MCP; mcp == nil || !reflect.DeepEqual(*mcp, []string{"gog"}) {
		t.Errorf("work.mcp = %v, want materialized [gog]", mcp)
	}
	// The base list must be untouched (Resolve aliases the base slice — the copy
	// in effectiveList is what protects it).
	if !reflect.DeepEqual(c.MCP, []string{"gog", "slack"}) {
		t.Errorf("base mcp corrupted = %v, want [gog slack]", c.MCP)
	}
	// Removing a value that isn't in the effective list is a no-op and must NOT
	// materialize an override on a fresh profile.
	if c.RemoveProfileMCP("fresh", "absent") {
		t.Error("RemoveProfileMCP of an absent value should report no change")
	}
	if p, ok := c.Profiles["fresh"]; ok && p.MCP != nil {
		t.Errorf("fresh.mcp = %v, want nil (no-op must not materialize)", p.MCP)
	}
	// Adding to an inheriting profile starts from the effective (base) list.
	if !c.AddProfileMCP("work2", "pio") {
		t.Fatal("AddProfileMCP should report a change")
	}
	if mcp := c.Profiles["work2"].MCP; mcp == nil || !reflect.DeepEqual(*mcp, []string{"gog", "slack", "pio"}) {
		t.Errorf("work2.mcp = %v, want [gog slack pio]", mcp)
	}
}

// TestRemoveProfileMCPLastInheritedPersistsEmpty is the tri-state round-trip
// gate: removing the LAST inherited mcp value (base=[gog], remove gog) must
// Save+Load as an explicit EMPTY list for that profile — NOT revert to the
// inherited [gog]. A non-pointer `omitempty` slice regresses this: the empty
// list is dropped on Save and re-inherits on Load.
func TestRemoveProfileMCPLastInheritedPersistsEmpty(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", t.TempDir()+"/config.toml")
	c := &Config{MCP: []string{"gog"}}
	if !c.RemoveProfileMCP("work", "gog") {
		t.Fatal("RemoveProfileMCP should report a change removing the last inherited value")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p, ok := got.Profiles["work"]
	if !ok {
		t.Fatalf("profile 'work' lost on round-trip: %+v", got.Profiles)
	}
	if p.MCP == nil {
		t.Fatal("work.mcp reverted to nil (inherit) — can't disable an inherited server")
	}
	if len(*p.MCP) != 0 {
		t.Errorf("work.mcp = %v, want explicit empty []", *p.MCP)
	}
	// And the resolved profile really sees an EMPTY mcp, not the base [gog].
	if rmcp := got.Resolve("work").MCP; len(rmcp) != 0 {
		t.Errorf("Resolve(work).MCP = %v, want empty (inherited server disabled)", rmcp)
	}
}

func baseConfig() *Config {
	c := &Config{
		MCP:              []string{"gog"},
		GogAccount:       "me@personal.com",
		KnowledgeBundles: []string{"/kb/personal"},
	}
	c.Kits.Stack = []string{"../personal-overlay/kit"}
	c.Profiles = map[string]Profile{
		"work": {
			GogAccount:       "me@work.com",
			MCP:              slp("gog", "slack"),
			KnowledgeBundles: slp("/kb/work"),
		},
	}
	work := c.Profiles["work"]
	work.Kits.Stack = slp("../work-overlay/kit")
	c.Profiles["work"] = work
	return c
}

func TestResolveDefaultReturnsBase(t *testing.T) {
	c := baseConfig()
	for _, name := range []string{"", "default", "nonexistent"} {
		got := c.Resolve(name)
		if got.GogAccount != "me@personal.com" {
			t.Errorf("Resolve(%q).GogAccount = %q, want base", name, got.GogAccount)
		}
		if !reflect.DeepEqual(got.MCP, []string{"gog"}) {
			t.Errorf("Resolve(%q).MCP = %v, want base [gog]", name, got.MCP)
		}
	}
}

func TestResolveWorkOverrides(t *testing.T) {
	c := baseConfig()
	got := c.Resolve("work")
	if got.GogAccount != "me@work.com" {
		t.Errorf("GogAccount = %q, want me@work.com", got.GogAccount)
	}
	if !reflect.DeepEqual(got.MCP, []string{"gog", "slack"}) {
		t.Errorf("MCP = %v, want [gog slack]", got.MCP)
	}
	if !reflect.DeepEqual(got.KnowledgeBundles, []string{"/kb/work"}) {
		t.Errorf("KnowledgeBundles = %v, want [/kb/work]", got.KnowledgeBundles)
	}
	if !reflect.DeepEqual(got.Kits.Stack, []string{"../work-overlay/kit"}) {
		t.Errorf("Kits.Stack = %v, want work overlay", got.Kits.Stack)
	}
}

func TestResolveDoesNotMutateReceiver(t *testing.T) {
	c := baseConfig()
	_ = c.Resolve("work")
	if c.GogAccount != "me@personal.com" || !reflect.DeepEqual(c.MCP, []string{"gog"}) {
		t.Errorf("Resolve mutated the base config: gog=%q mcp=%v", c.GogAccount, c.MCP)
	}
}

func TestResolveInheritsAbsentFields(t *testing.T) {
	c := baseConfig()
	// A profile that only overrides gog inherits base mcp + bundles.
	c.Profiles["mini"] = Profile{GogAccount: "mini@x.com"}
	got := c.Resolve("mini")
	if got.GogAccount != "mini@x.com" {
		t.Errorf("GogAccount = %q, want mini@x.com", got.GogAccount)
	}
	if !reflect.DeepEqual(got.MCP, []string{"gog"}) {
		t.Errorf("MCP = %v, want inherited [gog]", got.MCP)
	}
	if !reflect.DeepEqual(got.KnowledgeBundles, []string{"/kb/personal"}) {
		t.Errorf("bundles = %v, want inherited", got.KnowledgeBundles)
	}
}

func TestResolveEmptyListReplaces(t *testing.T) {
	c := baseConfig()
	// A present-but-empty mcp list REPLACES (disables mcp for this profile).
	c.Profiles["quiet"] = Profile{MCP: slp()}
	got := c.Resolve("quiet")
	if len(got.MCP) != 0 {
		t.Errorf("MCP = %v, want empty (present empty list replaces)", got.MCP)
	}
}

// TestProfileEmptyReplaceRoundTrips: a present-but-empty override (an explicit
// REPLACE that disables mcp) must survive Save+Load as a non-nil empty pointer,
// not collapse to nil (inherit).
func TestProfileEmptyReplaceRoundTrips(t *testing.T) {
	t.Setenv("PI_STACK_CONFIG", t.TempDir()+"/config.toml")
	c := &Config{MCP: []string{"gog", "slack"}}
	c.Profiles = map[string]Profile{"quiet": {MCP: slp()}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := got.Profiles["quiet"]
	if p.MCP == nil {
		t.Fatal("quiet.mcp reverted to nil (inherit); a present-empty REPLACE was dropped")
	}
	if len(*p.MCP) != 0 {
		t.Errorf("quiet.mcp = %v, want explicit empty []", *p.MCP)
	}
}

func TestAllKnowledgeBundlesUnion(t *testing.T) {
	c := baseConfig()
	got := c.AllKnowledgeBundles()
	want := []string{"/kb/personal", "/kb/work"}
	// order: base first, then profile; but map iteration is unordered for
	// multiple profiles — here only one profile, so it's deterministic.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AllKnowledgeBundles() = %v, want %v", got, want)
	}
}

func TestAllKnowledgeBundlesDedup(t *testing.T) {
	c := &Config{KnowledgeBundles: []string{"/kb/shared"}}
	c.Profiles = map[string]Profile{"work": {KnowledgeBundles: slp("/kb/shared", "/kb/work")}}
	got := c.AllKnowledgeBundles()
	if len(got) != 2 {
		t.Errorf("AllKnowledgeBundles() = %v, want 2 unique", got)
	}
}

func TestProfileNames(t *testing.T) {
	c := baseConfig()
	c.Profiles["personal"] = Profile{}
	got := c.ProfileNames()
	if got[0] != "default" {
		t.Errorf("ProfileNames()[0] = %q, want default first", got[0])
	}
	want := []string{"default", "personal", "work"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProfileNames() = %v, want %v", got, want)
	}
}
