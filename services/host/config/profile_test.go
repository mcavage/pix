package config

import (
	"reflect"
	"testing"
)

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
			MCP:              []string{"gog", "slack"},
			KnowledgeBundles: []string{"/kb/work"},
		},
	}
	work := c.Profiles["work"]
	work.Kits.Stack = []string{"../work-overlay/kit"}
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
	c.Profiles["quiet"] = Profile{MCP: []string{}}
	got := c.Resolve("quiet")
	if len(got.MCP) != 0 {
		t.Errorf("MCP = %v, want empty (present empty list replaces)", got.MCP)
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
	c.Profiles = map[string]Profile{"work": {KnowledgeBundles: []string{"/kb/shared", "/kb/work"}}}
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
