package secret

import "testing"

// TestToolKeysSyncButDoNotRoute pins the split between a key that buys a MODEL
// and a key that buys a CAPABILITY.
//
// Both need identical secret handling: seeded from an op:// ref, checked,
// mirrored into the sbx secret store, value never on disk. They differ in
// everything downstream. A tool key must never reach `pix models add` (which
// offers ProviderKeyRefOrder and would reject the name it just suggested), must
// never appear to the router, and must never be able to block a launch, since
// the "no model key" refusal is keyed on ModelProviders.
func TestToolKeysSyncButDoNotRoute(t *testing.T) {
	if len(ToolKeyRefOrder) == 0 {
		t.Fatal("ToolKeyRefOrder is empty: the web-search key wiring was dropped")
	}
	for _, tool := range ToolKeyRefOrder {
		// It IS mirrored: the sync set is the union of both lists.
		name, mirrored := providerKeyRefs[tool.EnvVar]
		if !mirrored {
			t.Errorf("%s is not in the sync set, so `pix secret sync` would skip it", tool.EnvVar)
		}
		if name != tool.Name {
			t.Errorf("%s mirrors to %q, want %q", tool.EnvVar, name, tool.Name)
		}
		// It is NOT routable.
		if isModelProviderKey(tool.EnvVar) {
			t.Errorf("%s reports as a model-provider key: `pix models add %s` would be offered and then rejected", tool.EnvVar, tool.Name)
		}
		for _, p := range ProviderKeyRefOrder {
			if p.EnvVar == tool.EnvVar || p.Name == tool.Name {
				t.Errorf("%s leaked into ProviderKeyRefOrder (the routing/`models add` set)", tool.EnvVar)
			}
		}
		for _, m := range ModelProviders {
			if m == tool.Name {
				t.Errorf("%s leaked into ModelProviders: a missing web-search key could then refuse a launch", tool.Name)
			}
		}
	}
	// The model keys are unchanged and still routable.
	for _, p := range ProviderKeyRefOrder {
		if !isModelProviderKey(p.EnvVar) {
			t.Errorf("%s must stay a model-provider key", p.EnvVar)
		}
		if _, mirrored := providerKeyRefs[p.EnvVar]; !mirrored {
			t.Errorf("%s dropped out of the sync set", p.EnvVar)
		}
	}
}
