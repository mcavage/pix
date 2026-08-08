package main

// TestHostAutoserveConfigKey moved back from the service package. Its subject
// is the CONFIG key -- set/unset through provision.ApplyConfigChange, read through
// provision.ConfigValue -- not the daemon. It followed serve_reload_test.go out by
// accident of file layout, and pulling it back was cheaper and more honest than
// copying two config helpers into a capability that has no business owning
// them.

import (
	"testing"

	"pix/host/config"
	"pix/host/workflow/provision"
)

// host.autoserve is a real config key: set/unset via provision.ApplyConfigChange, read
// via provision.ConfigValue, defaulting to true (nil pointer).
func TestHostAutoserveConfigKey(t *testing.T) {
	cfg := &config.Config{}
	if got, _ := provision.ConfigValue(cfg, "host.autoserve"); got != "true" {
		t.Errorf("default host.autoserve = %q, want true", got)
	}
	if _, err := provision.ApplyConfigChange(cfg, false, "host.autoserve", []string{"false"}); err != nil {
		t.Fatal(err)
	}
	if cfg.AutoserveEnabled() {
		t.Error("set false did not disable autoserve")
	}
	if got, _ := provision.ConfigValue(cfg, "host.autoserve"); got != "false" {
		t.Errorf("host.autoserve = %q, want false", got)
	}
	if _, err := provision.ApplyConfigChange(cfg, true, "host.autoserve", nil); err != nil {
		t.Fatal(err)
	}
	if cfg.Host.Autoserve != nil {
		t.Error("unset must return to nil (inherit the default, never petrify a bool)")
	}
	if _, err := provision.ApplyConfigChange(cfg, false, "host.autoserve", []string{"maybe"}); err == nil {
		t.Error("non-boolean accepted")
	}
}
