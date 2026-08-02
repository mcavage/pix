// setup_pack_activation_test.go — setupHostPhase activating an existing
// migrated default pack. The subject is the setup workflow, so it lives here;
// it moved out of pack/ with the rest and moved back when it turned out to need
// setup's own stepEnv fixture.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/secret"
	"pix/host/workflow/pack"
)

// setupHostPhase must activate an ALREADY-EXISTING default pack (e.g. one
// landed by the legacy migration, or discovered from a prior run) when
// cfg.Pack is empty — not only a brand-new one created via pack.RunPackNew.
func TestSetupHostPhase_ActivatesExistingMigratedDefaultPack_WhenCfgPackEmpty(t *testing.T) {
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))

	// Simulate a default pack that already exists (as if migrated or created by
	// an earlier run) but whose activation never landed: cfg.Pack is empty.
	root := filepath.Join(data, "pix", "default")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pack.WriteManifest(root, pack.Manifest{Name: "default", Schema: 1}); err != nil {
		t.Fatal(err)
	}

	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\nOPENAI_API_KEY=op://v/openai/key\nGEMINI_API_KEY=op://v/gemini/key\n"
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	// stepEnv points PIX_CONFIG/XDG_STATE_HOME/XDG_DATA_HOME at ITS OWN
	// temp dirs (overriding what we set above); redirect them back to cfgDir /
	// data so the real config.Load/Save AND the pre-created default pack this
	// test asserts on both land where we expect.
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))
	t.Setenv("XDG_DATA_HOME", data)
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := secret.RecordSyncedRefWithDigest(envVar, ref, secret.SecretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false); err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Pack != "" {
		t.Errorf("cfg.Pack after setup = %q; normal setup must not introduce or activate packs", cfg.Pack)
	}
}

// setupHostPhase must FAIL (propagate the error) when activating the default
// pack fails (cfg.Save error) — it must never report success while cfg.Pack
// still points nowhere.
func TestSetupHostPhase_PackActivationFailure_FailsSetup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}
	data := t.TempDir()
	cfgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))

	root := filepath.Join(data, "pix", "default")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := pack.WriteManifest(root, pack.Manifest{Name: "default", Schema: 1}); err != nil {
		t.Fatal(err)
	}

	refs := "ANTHROPIC_API_KEY=op://v/anthropic/key\nOPENAI_API_KEY=op://v/openai/key\nGEMINI_API_KEY=op://v/gemini/key\n"
	env, _ := stepEnv(t, refs, "anthropic openai google", "sk-val")
	// stepEnv points PIX_CONFIG/XDG_STATE_HOME/XDG_DATA_HOME at ITS OWN
	// temp dirs (overriding what we set above); redirect them back to cfgDir /
	// data so we chmod the SAME directory config.Save() actually writes into
	// and the pre-created pack above is the one setupHostPhase resolves.
	t.Setenv("PIX_CONFIG", filepath.Join(cfgDir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(cfgDir, "state"))
	t.Setenv("XDG_DATA_HOME", data)
	for envVar, ref := range map[string]string{
		"ANTHROPIC_API_KEY": "op://v/anthropic/key",
		"OPENAI_API_KEY":    "op://v/openai/key",
		"GEMINI_API_KEY":    "op://v/gemini/key",
	} {
		if err := secret.RecordSyncedRefWithDigest(envVar, ref, secret.SecretDigestHex("sk-val")); err != nil {
			t.Fatal(err)
		}
	}
	// Config must exist on disk before it becomes unwritable, so setupHostPhase's
	// own earlier config.Load()/writes have already succeeded and only the pack
	// activation's cfg.Save is what fails.
	if cfg, err := config.Load(); err != nil {
		t.Fatal(err)
	} else if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(cfgDir, 0o755) })

	var out bytes.Buffer
	err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, false)
	_ = os.Chmod(cfgDir, 0o755) // restore before any later cleanup/log reads
	if err == nil {
		t.Fatal("setup must fail when default-pack activation fails")
	}
}
