package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A config.toml written before W1 U01a can name a service that no longer
// exists. Loading it must not fail and must not leave the retired name in the
// resolved set — the same tolerance the earlier gws/gws-token retirement got.
func TestLoad_TolerateRetiredServiceNames(t *testing.T) {
	for name, tc := range map[string]struct {
		body string
		want []string
	}{
		"retired only falls back to defaults": {"services = [\"knowledge\"]\n", DefaultServices},
		"retired alongside a live service":    {"services = [\"memory\", \"knowledge\"]\n", []string{"memory"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFrom(path)
			if err != nil {
				t.Fatalf("LoadFrom(stale services): %v", err)
			}
			if !stringSlicesEqual(cfg.Services, tc.want) {
				t.Errorf("Services = %v, want %v", cfg.Services, tc.want)
			}
		})
	}
}
